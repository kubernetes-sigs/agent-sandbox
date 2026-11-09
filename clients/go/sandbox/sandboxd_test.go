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
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// newReadySandboxdTestSandbox creates a RuntimeSandboxd Sandbox already
// "connected" to the given REST server URL.
func newReadySandboxdTestSandbox(serverURL string) *Sandbox {
	return newReadySandboxdTestSandboxWith(serverURL, nil)
}

// newReadySandboxdTestSandboxWith is newReadySandboxdTestSandbox with
// tweak applied to the Options first.
func newReadySandboxdTestSandboxWith(serverURL string, tweak func(*Options)) *Sandbox {
	opts := Options{
		WarmPoolName:      "test-warmpool",
		Namespace:         "default",
		APIURL:            serverURL,
		Runtime:           RuntimeSandboxd,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Quiet:             true,
	}
	if tweak != nil {
		tweak(&opts)
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
func startFakeProcessServer(t *testing.T, sb *Sandbox, svc processv1.ProcessServiceServer) {
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
	if gotPath != "/v1/files/dir/script.py" {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("unexpected content type: %s", gotContentType)
	}
	if string(gotBody) != "print(1)" {
		t.Errorf("unexpected body: %q", gotBody)
	}
}

func TestSandboxdWrite_KeepsAbsolutePath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	if err := c.Write(context.Background(), "/workspace/a.txt", []byte("x")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if gotPath != "/v1/files/%2Fworkspace/a.txt" {
		t.Fatalf("path = %s, want /v1/files/%%2Fworkspace/a.txt", gotPath)
	}
}

func TestSandboxdPathErrorsIncludeOperation(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused")
	prefix := "sandbox[default/test-claim-abc123]"
	checks := []struct {
		name string
		err  error
		want string
	}{
		{"write", c.Write(context.Background(), "..", []byte("x")), prefix + `: write(".."):`},
		{"read", func() error { _, err := c.Read(context.Background(), "."); return err }(), prefix + `: read("."):`},
		{"list", func() error { _, err := c.List(context.Background(), "../x"); return err }(), prefix + `: list("../x"):`},
		{"delete", c.Delete(context.Background(), ".", false), prefix + `: delete("."):`},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", tc.err, tc.want)
			}
		})
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
	for _, p := range []string{"../etc/passwd", "dir/../../etc/passwd", "dir/../a.txt", "..", ".", "foo/.."} {
		err := c.Write(context.Background(), p, []byte("x"))
		if err == nil {
			t.Errorf("Write(%q) should be rejected client-side", p)
		}
	}
	if called {
		t.Error("traversal write must not reach the server")
	}
}

func TestSandboxdReadAndDelete_RejectRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("root path must not reach the server")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	if _, err := c.Read(context.Background(), "."); err == nil || !strings.Contains(err.Error(), "sandbox root") {
		t.Fatalf("Read(.): %v", err)
	}
	if err := c.Delete(context.Background(), "foo/..", true); err == nil || !strings.Contains(err.Error(), "sandbox root") {
		t.Fatalf("Delete(foo/..): %v", err)
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
		if r.URL.EscapedPath() != "/v1/files/notes/hello.txt" {
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
		if r.URL.EscapedPath() != "/v1/files/notes/hello.txt" {
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

func TestSandboxdFilesEndpointCollapsesDotSegments(t *testing.T) {
	cases := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{path: "dir/script.py", want: "v1/files/dir/script.py"},
		{path: "./a.txt", want: "v1/files/a.txt"},
		{path: "dir//a.txt", want: "v1/files/dir/a.txt"},
		{path: "dir/", want: "v1/files/dir"},
		{path: "/", want: "v1/files/"},
		{path: ".", want: "v1/files/"},
		{path: "/workspace/a.txt", want: "v1/files/%2Fworkspace/a.txt"},
		{path: "/dir", want: "v1/files/%2Fdir"},
		{path: "//dir//a.txt", want: "v1/files/%2Fdir/a.txt"},
		{path: "dir/../a.txt", wantErr: true},
		{path: "../escape", wantErr: true},
		{path: "dir/../../escape", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := filesEndpoint(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("filesEndpoint(%q) = %q, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("filesEndpoint(%q) error: %v", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("filesEndpoint(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestSandboxdFileOps_Requests(t *testing.T) {
	tests := []struct {
		name       string
		call       func(*Sandbox) error
		wantMethod string
		wantPath   string
		wantQuery  string
	}{
		{"mkdir", func(c *Sandbox) error { return c.Mkdir(context.Background(), "a/b", false) },
			http.MethodPut, "/v1/files/a/b", "type=directory"},
		{"mkdir parents", func(c *Sandbox) error { return c.Mkdir(context.Background(), "a/b", true) },
			http.MethodPut, "/v1/files/a/b", "parents=true&type=directory"},
		{"move", func(c *Sandbox) error { return c.Move(context.Background(), "a", "dir/b c", false) },
			http.MethodPost, "/v1/files/a:move", "destination=dir%2Fb+c"},
		{"copy overwrite", func(c *Sandbox) error { return c.Copy(context.Background(), "a", "b", true) },
			http.MethodPost, "/v1/files/a:copy", "destination=b&overwrite=true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath, gotQuery string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotQuery = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			if err := tt.call(newReadySandboxdTestSandbox(server.URL)); err != nil {
				t.Fatalf("error: %v", err)
			}
			if gotMethod != tt.wantMethod || gotPath != tt.wantPath || gotQuery != tt.wantQuery {
				t.Errorf("got %s %s?%s, want %s %s?%s", gotMethod, gotPath, gotQuery, tt.wantMethod, tt.wantPath, tt.wantQuery)
			}
		})
	}
}

func TestSandboxdFileOps_RetryDefaults(t *testing.T) {
	tests := []struct {
		name      string
		call      func(*Sandbox) error
		wantCalls int32
	}{
		{"mkdir retries", func(c *Sandbox) error { return c.Mkdir(context.Background(), "d", false) }, 2},
		{"move does not retry", func(c *Sandbox) error { return c.Move(context.Background(), "a", "b", false) }, 1},
		{"copy does not retry", func(c *Sandbox) error { return c.Copy(context.Background(), "a", "b", false) }, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			err := tt.call(newReadySandboxdTestSandbox(server.URL))
			if tt.wantCalls == 2 && err != nil {
				t.Fatalf("expected the retry to succeed, got: %v", err)
			}
			if tt.wantCalls == 1 && err == nil {
				t.Fatal("expected the 503 to be returned without a retry")
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("server saw %d calls, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestSandboxdFileOps_Validation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an invalid call must not reach the server")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadySandboxdTestSandbox(server.URL)
	if err := c.Mkdir(context.Background(), "", false); err == nil {
		t.Error("Mkdir with an empty path: expected error")
	}
	if err := c.Move(context.Background(), "", "b", false); err == nil {
		t.Error("Move with an empty source: expected error")
	}
	if err := c.Copy(context.Background(), "a", "", false); err == nil {
		t.Error("Copy with an empty destination: expected error")
	}
}

func TestSandboxdFileOps_LegacyRuntimeUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("legacy calls must not reach the server")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	for name, err := range map[string]error{
		"Mkdir": c.Mkdir(context.Background(), "d", false),
		"Move":  c.Move(context.Background(), "a", "b", false),
		"Copy":  c.Copy(context.Background(), "a", "b", false),
	} {
		if !errors.Is(err, ErrUnsupportedByRuntime) {
			t.Errorf("%s: expected ErrUnsupportedByRuntime, got: %v", name, err)
		}
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
	_, err := c.Read(context.Background(), "secret/file.txt")
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

func TestSandboxdRun_ForwardsEnvAndWorkingDir(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{}}
	startFakeProcessServer(t, c, svc)

	// Repeated WithEnv calls accumulate and the later value wins.
	_, err := c.Run(context.Background(), "env",
		WithEnv(map[string]string{"A": "1", "B": "old"}),
		WithEnv(map[string]string{"B": "2"}),
		WithWorkingDir("proj"))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	cfg := svc.lastRequest.GetConfig()
	if got := cfg.GetEnvVars(); len(got) != 2 || got["A"] != "1" || got["B"] != "2" {
		t.Errorf("env = %v, want A=1 B=2", got)
	}
	if cfg.Cwd == nil || *cfg.Cwd != "proj" {
		t.Errorf("cwd = %v, want proj", cfg.Cwd)
	}

	// Without options, nothing extra is sent.
	if _, err := c.Run(context.Background(), "env"); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	cfg = svc.lastRequest.GetConfig()
	if len(cfg.GetEnvVars()) != 0 || cfg.Cwd != nil {
		t.Errorf("unexpected env/cwd by default: %v %v", cfg.GetEnvVars(), cfg.Cwd)
	}
}

func TestSandboxdRun_RejectsInvalidEnvKey(t *testing.T) {
	for _, key := range []string{"", "A=B"} {
		t.Run(key, func(t *testing.T) {
			c := newReadySandboxdTestSandbox("http://unused.invalid")
			svc := &fakeProcessService{response: &processv1.ExecuteResponse{}}
			startFakeProcessServer(t, c, svc)

			_, err := c.Run(context.Background(), "env", WithEnv(map[string]string{key: "v"}))
			if err == nil || !strings.Contains(err.Error(), "invalid env key") {
				t.Fatalf("expected invalid env key error, got: %v", err)
			}
			if svc.lastRequest != nil {
				t.Error("an invalid env key must not reach sandboxd")
			}
		})
	}
}

func TestRun_LegacyRuntimeRejectsEnvAndWorkingDir(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unsupported option must not reach the server")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	for name, opt := range map[string]CallOption{
		"env": WithEnv(map[string]string{"A": "1"}),
		"cwd": WithWorkingDir("proj"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Run(context.Background(), "env", opt); !errors.Is(err, ErrUnsupportedByRuntime) {
				t.Fatalf("expected ErrUnsupportedByRuntime, got: %v", err)
			}
		})
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

// hangingProcessService never answers Execute until the call is cancelled.
type hangingProcessService struct {
	processv1.UnimplementedProcessServiceServer
}

func (hangingProcessService) Execute(ctx context.Context, _ *processv1.ExecuteRequest) (*processv1.ExecuteResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSandboxdRun_AppliesRequestTimeout(t *testing.T) {
	c := newReadySandboxdTestSandbox("http://unused.invalid")
	c.connector.requestTimeout = 200 * time.Millisecond
	startFakeProcessServer(t, c, hangingProcessService{})

	done := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), "sleep 999")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "DeadlineExceeded") {
			t.Fatalf("expected DeadlineExceeded, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not apply RequestTimeout to the sandboxd Execute call")
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

// Once the pod port-forward dies, Run and Start must report ErrNotReady
// wrapping ErrPortForwardDied, as REST calls do, so callers know to call
// Open. The published gRPC target is a local port of the dead tunnel.
func TestSandboxdRun_PodTunnelDeathReturnsErrNotReady(t *testing.T) {
	opts := defaultTestOpts()
	opts.APIURL = ""
	opts.Runtime = RuntimeSandboxd
	c, _, _ := newTestSandbox(opts)

	// serve stands in for one tunnel's gRPC local port.
	serve := func(stdout string) (*grpc.Server, uint16) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen: %v", err)
		}
		srv := grpc.NewServer()
		processv1.RegisterProcessServiceServer(srv, &fakeProcessService{response: &processv1.ExecuteResponse{Stdout: []byte(stdout)}})
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		return srv, uint16(lis.Addr().(*net.TCPAddr).Port)
	}
	forwarder := func(grpcLocal uint16) *fakeForwarder {
		return &fakeForwarder{
			ports: []portforward.ForwardedPort{
				{Local: 12345, Remote: uint16(opts.SandboxdRESTPort)},
				{Local: grpcLocal, Remote: uint16(opts.SandboxdGRPCPort)},
			},
			signalReady: true,
			die:         make(chan error, 1),
			returned:    make(chan struct{}),
		}
	}

	grpcServer, grpcLocal := serve("first\n")
	fw := forwarder(grpcLocal)
	stub := newTestPortForwardTunnel(fw)
	pts := c.connector.strategy.(*podTunnelStrategy)
	pts.tunnel.coreClient = fakekube.NewSimpleClientset().CoreV1() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	pts.tunnel.restConfig = &rest.Config{}
	pts.tunnel.dial = stub.dial
	pts.tunnel.newForwarder = stub.newForwarder

	c.setState(&sandboxState{SandboxName: "sb", PodName: "sb"})
	if err := c.connector.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	if _, err := c.Run(context.Background(), "echo hi"); err != nil {
		t.Fatalf("Run() before tunnel death: %v", err)
	}
	deadConn, err := c.connector.GRPCConn()
	if err != nil {
		t.Fatalf("GRPCConn() before tunnel death: %v", err)
	}

	// client-go closes its local listeners when it loses the pod connection.
	grpcServer.Stop()
	fw.die <- portforward.ErrLostConnectionToPod
	waitForwardReturned(t, fw)
	deadline := time.Now().Add(2 * time.Second)
	for c.IsReady() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the monitor to report tunnel death")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err = c.Run(context.Background(), "echo hi")
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, ErrPortForwardDied) {
		t.Fatalf("expected ErrNotReady wrapping ErrPortForwardDied, got: %v", err)
	}
	if _, err := c.Commands().Start(context.Background(), "echo hi"); !errors.Is(err, ErrNotReady) || !errors.Is(err, ErrPortForwardDied) {
		t.Errorf("expected Start to return ErrNotReady wrapping ErrPortForwardDied, got: %v", err)
	}
	// SetLastError closes the conn after releasing the connector lock.
	deadline = time.Now().Add(2 * time.Second)
	for deadConn.GetState() != connectivity.Shutdown {
		if time.Now().After(deadline) {
			t.Fatalf("expected the dead tunnel's gRPC conn to be closed, got state %s", deadConn.GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Reconnecting, as Open does, must publish the new tunnel's target.
	_, grpcLocal = serve("second\n")
	fw2 := forwarder(grpcLocal)
	pts.tunnel.newForwarder = newTestPortForwardTunnel(fw2).newForwarder
	if err := c.connector.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() after tunnel death: %v", err)
	}
	t.Cleanup(func() {
		_ = pts.tunnel.Close()
		waitForwardReturned(t, fw2)
	})
	result, err := c.Run(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("Run() after reconnect: %v", err)
	}
	if result.Stdout != "second\n" {
		t.Errorf("expected Run to reach the new tunnel, got stdout %q", result.Stdout)
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

				err := report.call(context.Background(), newReadySandboxdTestSandbox(server.URL))
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

func TestSandboxdRuntimeReports_RetriesOnlyWhenAsked(t *testing.T) {
	for _, report := range runtimeReports {
		for _, tt := range []struct {
			name string
			opts []CallOption
			want int32
		}{
			{"default is one attempt", nil, 1},
			{"opt in to retries", []CallOption{WithMaxAttempts(3)}, 3},
		} {
			t.Run(report.name+"/"+tt.name, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer server.Close()

				_ = report.call(context.Background(), newReadySandboxdTestSandbox(server.URL), tt.opts...)
				if got := requests.Load(); got != tt.want {
					t.Errorf("requests = %d, want %d", got, tt.want)
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
	err := opts.validateCommon()
	if err == nil || !strings.Contains(err.Error(), "Gateway mode does not support sandboxd yet") {
		t.Fatalf("expected Gateway mode to say it does not support sandboxd yet, got %v", err)
	}
}

func TestOptions_SandboxdAuthTokenValidation(t *testing.T) {
	tests := []struct {
		name    string
		runtime Runtime
		token   string
		wantErr string
	}{
		{name: "valid", runtime: RuntimeSandboxd, token: "s3cret-Tok_en.1~"},
		{name: "legacy runtime", runtime: RuntimeLegacyPython, token: "tok", wantErr: "requires sandboxd"},
		{name: "space", runtime: RuntimeSandboxd, token: "a b", wantErr: "printable ASCII"},
		{name: "newline", runtime: RuntimeSandboxd, token: "tok\n", wantErr: "printable ASCII"},
		{name: "non-ASCII", runtime: RuntimeSandboxd, token: "tök", wantErr: "printable ASCII"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{WarmPoolName: "wp", Runtime: tt.runtime, SandboxdAuthToken: tt.token}
			opts.setDefaults()
			err := opts.validateCommon()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCommon() error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			if strings.Contains(err.Error(), tt.token) {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestSandboxdAuthToken_SentAsBearerOnREST(t *testing.T) {
	var gotAuth, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get(headerSandboxdToken)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()

	sb := newReadySandboxdTestSandboxWith(server.URL, func(o *Options) { o.SandboxdAuthToken = "tok" })
	if _, err := sb.Health(context.Background()); err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	if gotAuth != "Bearer tok" || gotToken != "" {
		t.Errorf("got Authorization=%q X-Sandboxd-Token=%q, want Bearer tok and none", gotAuth, gotToken)
	}
}

func TestSandboxdAuthToken_OmittedWhenUnset(t *testing.T) {
	var gotAuth, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get(headerSandboxdToken)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()

	sb := newReadySandboxdTestSandbox(server.URL)
	if _, err := sb.Health(context.Background()); err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	if gotAuth != "" || gotToken != "" {
		t.Errorf("got Authorization=%q X-Sandboxd-Token=%q, want none", gotAuth, gotToken)
	}
}

func TestSandboxdAuthToken_SentAsBearerOnGRPC(t *testing.T) {
	sb := newReadySandboxdTestSandboxWith("http://unused", func(o *Options) { o.SandboxdAuthToken = "tok" })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	var gotAuth []string
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		gotAuth = md.Get("authorization")
		return h(ctx, req)
	}))
	processv1.RegisterProcessServiceServer(grpcServer, &fakeProcessService{response: &processv1.ExecuteResponse{}})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	sb.connector.SetGRPCTarget(lis.Addr().String())

	if _, err := sb.Run(context.Background(), "true"); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer tok" {
		t.Errorf("got authorization metadata %q, want [Bearer tok]", gotAuth)
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
