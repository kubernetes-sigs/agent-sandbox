// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// newReadySandboxdTestSandbox creates a RuntimeSandboxd Sandbox already
// "connected" to the given REST server URL.
func newReadySandboxdTestSandbox(serverURL string) *Sandbox {
	opts := Options{
		WarmPoolName:      "test-warmpool",
		Namespace:         "default",
		APIURL:            serverURL,
		Runtime:           RuntimeSandboxd,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Quiet:             true,
	}
	opts.setDefaults()

	k8s := &K8sHelper{Log: opts.Logger}
	opts.K8sHelper = k8s
	sb, err := New(context.Background(), opts)
	if err != nil {
		panic("newReadySandboxdTestSandbox: " + err.Error())
	}
	sb.connector.mu.Lock()
	sb.connector.baseURL = serverURL
	sb.connector.sandboxID = "test-claim-abc123"
	sb.connector.backoffScale = 0.001
	sb.connector.mu.Unlock()
	sb.mu.Lock()
	sb.claimName = "test-claim-abc123"
	sb.mu.Unlock()
	return sb
}

// fakeProcessService records Execute requests and returns a canned response.
type fakeProcessService struct {
	processv1.UnimplementedProcessServiceServer
	lastRequest *processv1.ExecuteRequest
	response    *processv1.ExecuteResponse
	err         error
}

func (f *fakeProcessService) Execute(_ context.Context, req *processv1.ExecuteRequest) (*processv1.ExecuteResponse, error) {
	f.lastRequest = req
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// startFakeProcessServer runs a gRPC ProcessService on an ephemeral loopback
// port and wires the sandbox's connector at it.
func startFakeProcessServer(t *testing.T, sb *Sandbox, svc *fakeProcessService) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	processv1.RegisterProcessServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = lis.Close()
	})
	sb.connector.SetGRPCTarget(lis.Addr().String())
}

// ---------------------------------------------------------------------------
// Files: REST /v1 surface
// ---------------------------------------------------------------------------

func TestSandboxdWrite_PutsRawBody(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	if err := c.Write(context.Background(), "dir/script.py", []byte("print(1)")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("expected PUT, got %s", gotMethod)
	}
	if gotPath != "/v1/files/dir%2Fscript.py" {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("unexpected content type: %s", gotContentType)
	}
	if string(gotBody) != "print(1)" {
		t.Errorf("unexpected body: %q", gotBody)
	}
}

func TestSandboxdWrite_NoRouterHeaders(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	if err := c.Write(context.Background(), "f.txt", []byte("x")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	for _, h := range []string{headerSandboxID, headerSandboxNamespace, headerSandboxPort, headerSandboxPodIP} {
		if gotHeaders.Get(h) != "" {
			t.Errorf("router header %s must not be sent to sandboxd, got %q", h, gotHeaders.Get(h))
		}
	}
}

func TestSandboxdRead_GetsFileBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/files/notes%2Fhello.txt" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	data, err := c.Read(context.Background(), "notes/hello.txt")
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestSandboxdList_ParsesDirectoryListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": "/notes",
			"entries": []map[string]any{
				{"name": "a.txt", "size": 5, "type": "file", "modified_at": "2026-08-06T10:00:00Z", "mode": "0644"},
				{"name": "sub", "size": 0, "type": "directory", "modified_at": "2026-08-06T11:00:00Z", "mode": "0755"},
			},
		})
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	entries, err := c.List(context.Background(), "notes")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name != "a.txt" || entries[0].Type != FileTypeFile || entries[0].Mode != "0644" {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}
	want := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	if !entries[0].ModTime.Equal(want) {
		t.Errorf("unexpected ModTime: %v (want %v)", entries[0].ModTime, want)
	}
	if entries[1].Type != FileTypeDirectory {
		t.Errorf("unexpected second entry: %+v", entries[1])
	}
}

func TestSandboxdExists_HeadStatusCodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		if strings.Contains(r.URL.EscapedPath(), "present") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	exists, err := c.Exists(context.Background(), "present.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if !exists {
		t.Error("expected present.txt to exist")
	}
	exists, err = c.Exists(context.Background(), "absent.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if exists {
		t.Error("expected absent.txt to not exist")
	}
}

func TestSandboxdDelete_SendsRecursiveQuery(t *testing.T) {
	var gotMethod, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	if err := c.Delete(context.Background(), "dir", true); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotQuery != "recursive=true" {
		t.Errorf("expected recursive=true query, got %q", gotQuery)
	}
}

func TestDelete_LegacyRuntimeUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("legacy delete must not reach the server")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	err := c.Delete(context.Background(), "f.txt", false)
	if !errors.Is(err, ErrUnsupportedByRuntime) {
		t.Fatalf("expected ErrUnsupportedByRuntime, got: %v", err)
	}
}

func TestSandboxdError_DecodesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "PERMISSION_DENIED",
			"message": "path traversal outside sandbox root is forbidden",
		})
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	_, err := c.Read(context.Background(), "../etc/passwd")
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected HTTPError, got: %v", err)
	}
	if httpErr.StatusCode != http.StatusForbidden {
		t.Errorf("unexpected status: %d", httpErr.StatusCode)
	}
	if !strings.Contains(httpErr.Body, "PERMISSION_DENIED") {
		t.Errorf("expected decoded APIError code in body, got: %q", httpErr.Body)
	}
}

// ---------------------------------------------------------------------------
// Commands: gRPC ProcessService surface
// ---------------------------------------------------------------------------

func TestSandboxdRun_ExecutesViaGRPC(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{
		ExitCode: 0,
		Stdout:   []byte("hello\n"),
		Stderr:   []byte(""),
	}}
	startFakeProcessServer(t, c, svc)

	result, err := c.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Stdout != "hello\n" || result.ExitCode != 0 {
		t.Errorf("unexpected result: %+v", result)
	}
	gotCmd := svc.lastRequest.GetConfig().GetCommand()
	if len(gotCmd) != 3 || gotCmd[0] != "/bin/sh" || gotCmd[1] != "-c" || gotCmd[2] != "echo hello" {
		t.Errorf("expected /bin/sh -c wrapping, got: %v", gotCmd)
	}
}

func TestSandboxdRun_NonZeroExitCode(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{
		ExitCode: 3,
		Stderr:   []byte("boom"),
	}}
	startFakeProcessServer(t, c, svc)

	result, err := c.Run(context.Background(), "exit 3")
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitCode != 3 || result.Stderr != "boom" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestSandboxdRun_GRPCErrorSurfacesCode(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeProcessService{err: status.Error(codes.NotFound, "command not found")}
	startFakeProcessServer(t, c, svc)

	_, err := c.Run(context.Background(), "definitely-not-a-binary")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "NotFound") {
		t.Errorf("expected gRPC code in error, got: %v", err)
	}
}

func TestSandboxdRun_NotConnected(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	// No gRPC target published (tunnel never connected).
	_, err := c.Run(context.Background(), "echo hi")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Options validation
// ---------------------------------------------------------------------------

func TestOptions_SandboxdRejectsGateway(t *testing.T) {
	opts := Options{
		WarmPoolName: "wp",
		Runtime:      RuntimeSandboxd,
		GatewayName:  "gw",
	}
	opts.setDefaults()
	if err := opts.validate(); err == nil {
		t.Fatal("expected validation error for RuntimeSandboxd + GatewayName")
	}
}

func TestOptions_SandboxdPortDefaults(t *testing.T) {
	opts := Options{WarmPoolName: "wp", Runtime: RuntimeSandboxd}
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		t.Fatalf("validate() error: %v", err)
	}
	if opts.SandboxdRESTPort != 8080 || opts.SandboxdGRPCPort != 9090 {
		t.Errorf("unexpected port defaults: rest=%d grpc=%d", opts.SandboxdRESTPort, opts.SandboxdGRPCPort)
	}
}
