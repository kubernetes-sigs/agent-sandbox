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
	"bytes"
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

func TestSandboxdWrite_RejectsTraversal(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	for _, p := range []string{"../etc/passwd", "dir/../../etc/passwd", ".."} {
		err := c.Write(context.Background(), p, []byte("x"))
		if err == nil {
			t.Errorf("Write(%q) should be rejected client-side", p)
		}
	}
	if called {
		t.Error("traversal write must not reach the server")
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

func TestSandboxdReadTo_GetsFileBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/files/notes%2Fhello.txt" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	var destination bytes.Buffer
	written, err := c.ReadTo(context.Background(), "notes/hello.txt", &destination)
	if err != nil {
		t.Fatalf("ReadTo() error: %v", err)
	}
	if written != 5 || destination.String() != "hello" {
		t.Fatalf("ReadTo() = (%d, %q), want (5, %q)", written, destination.String(), "hello")
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
	// The in-cluster strategy publishes a gRPC target on Open; before that,
	// Run is merely not ready.
	c, _, _ := newTestSandbox(inClusterTestOpts())
	_, err := c.Run(context.Background(), "echo hi")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got: %v", err)
	}
}

// APIURL addresses only the REST API, so no gRPC target is ever published:
// Run must fail permanently instead of reporting a retryable ErrNotReady.
func TestSandboxdRun_DirectURLUnsupported(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	_, err := c.Run(context.Background(), "echo hi")
	if !errors.Is(err, ErrUnsupportedByRuntime) {
		t.Fatalf("expected ErrUnsupportedByRuntime, got: %v", err)
	}
	if errors.Is(err, ErrNotReady) {
		t.Errorf("a permanent failure must not look retryable, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Runtime reports: GET /v1/health and /v1/metadata
// ---------------------------------------------------------------------------

// runtimeReports lists each sandboxd report call so every behavior below is
// checked for both without repeating the test body.
var runtimeReports = []struct {
	name string
	call func(context.Context, *Sandbox, ...CallOption) error
}{
	{"Health", func(ctx context.Context, s *Sandbox, o ...CallOption) error {
		_, err := s.Health(ctx, o...)
		return err
	}},
	{"Metadata", func(ctx context.Context, s *Sandbox, o ...CallOption) error {
		_, err := s.Metadata(ctx, o...)
		return err
	}},
}

func TestSandboxdHealth_ParsesReport(t *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		_, _ = io.WriteString(w, `{"status":"ok","uptime_seconds":42}`)
	}))
	defer server.Close()

	got, err := newReadySandboxdTestSandbox(server.URL).Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/health" {
		t.Errorf("unexpected request: %s %s", gotMethod, gotPath)
	}
	if got.Status != "ok" || got.UptimeSeconds != 42 {
		t.Errorf("unexpected health: %+v", got)
	}
}

func TestSandboxdMetadata_ParsesEnv(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]string
	}{
		{"values", `{"env":{"SANDBOX_ID":"abc","SANDBOX_REGION":"us"}}`, map[string]string{"SANDBOX_ID": "abc", "SANDBOX_REGION": "us"}},
		{"empty object", `{}`, map[string]string{}},
		{"null env", `{"env":null}`, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.EscapedPath()
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			got, err := newReadySandboxdTestSandbox(server.URL).Metadata(context.Background())
			if err != nil {
				t.Fatalf("Metadata() error: %v", err)
			}
			if gotMethod != http.MethodGet || gotPath != "/v1/metadata" {
				t.Errorf("unexpected request: %s %s", gotMethod, gotPath)
			}
			if got.Env == nil || len(got.Env) != len(tt.want) {
				t.Fatalf("unexpected env: %#v (want %#v)", got.Env, tt.want)
			}
			for k, v := range tt.want {
				if got.Env[k] != v {
					t.Errorf("env[%q] = %q, want %q", k, got.Env[k], v)
				}
			}
		})
	}
}

func TestSandboxdRuntimeReports_HTTPErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		code     string
		wantBody string
		wantErr  error
	}{
		{"decodes API error", http.StatusForbidden, "PERMISSION_DENIED", "PERMISSION_DENIED: nope", nil},
		{"shutting down", http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", ErrRetriesExhausted},
	}
	for _, report := range runtimeReports {
		for _, tt := range tests {
			t.Run(report.name+"/"+tt.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					_ = json.NewEncoder(w).Encode(sandboxdAPIError{Code: tt.code, Message: "nope"})
				}))
				defer server.Close()

				err := report.call(context.Background(), newReadySandboxdTestSandbox(server.URL), WithMaxAttempts(1))
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) {
					t.Fatalf("expected HTTPError, got: %v", err)
				}
				if httpErr.StatusCode != tt.status || !strings.Contains(httpErr.Body, tt.wantBody) {
					t.Errorf("unexpected HTTPError: %+v", httpErr)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("expected %v, got: %v", tt.wantErr, err)
				}
			})
		}
	}
}

func TestSandboxdRuntimeReports_BadBody(t *testing.T) {
	for _, report := range runtimeReports {
		t.Run(report.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "not json")
			}))
			defer server.Close()

			err := report.call(context.Background(), newReadySandboxdTestSandbox(server.URL))
			if err == nil || !strings.Contains(err.Error(), "failed to decode") {
				t.Fatalf("expected decode error, got: %v", err)
			}
		})
	}
}

func TestSandboxdRuntimeReports_LegacyRuntimeUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("legacy runtime must not reach the server")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	for _, report := range runtimeReports {
		t.Run(report.name, func(t *testing.T) {
			err := report.call(context.Background(), newReadyTestSandbox(server.URL))
			if !errors.Is(err, ErrUnsupportedByRuntime) {
				t.Fatalf("expected ErrUnsupportedByRuntime, got: %v", err)
			}
		})
	}
}

func TestSandboxdRuntimeReports_NotConnected(t *testing.T) {
	for _, report := range runtimeReports {
		t.Run(report.name, func(t *testing.T) {
			c, _, _ := newTestSandbox(inClusterTestOpts())
			if err := report.call(context.Background(), c); !errors.Is(err, ErrNotReady) {
				t.Fatalf("expected ErrNotReady, got: %v", err)
			}
		})
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
	if err := opts.validateCommon(); err == nil {
		t.Fatal("expected validation error for RuntimeSandboxd + GatewayName")
	}
}

func TestOptions_SandboxdPortDefaults(t *testing.T) {
	opts := Options{WarmPoolName: "wp", Runtime: RuntimeSandboxd}
	opts.setDefaults()
	if err := opts.validateCommon(); err != nil {
		t.Fatalf("validateCommon() error: %v", err)
	}
	if opts.SandboxdRESTPort != 8080 || opts.SandboxdGRPCPort != 9090 {
		t.Errorf("unexpected port defaults: rest=%d grpc=%d", opts.SandboxdRESTPort, opts.SandboxdGRPCPort)
	}
}
