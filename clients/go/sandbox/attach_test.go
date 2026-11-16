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
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// attachEndpoint is one host and port serving sandboxd's REST API and its
// gRPC ProcessService, as a platform endpoint in front of a sandbox does. It
// records the headers of every REST request and gRPC call.
type attachEndpoint struct {
	srv *httptest.Server

	mu          sync.Mutex
	restHeaders []http.Header
	grpcHeaders []http.Header
	files       map[string][]byte
}

func startAttachEndpoint(t *testing.T, useTLS bool, svc processv1.ProcessServiceServer) *attachEndpoint {
	t.Helper()
	grpcServer := grpc.NewServer()
	processv1.RegisterProcessServiceServer(grpcServer, svc)

	e := &attachEndpoint{files: map[string][]byte{}}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if req.ProtoMajor == 2 && strings.HasPrefix(req.Header.Get("Content-Type"), "application/grpc") {
			e.grpcHeaders = append(e.grpcHeaders, req.Header.Clone())
			e.mu.Unlock()
			grpcServer.ServeHTTP(w, req)
			e.mu.Lock()
			return
		}
		e.restHeaders = append(e.restHeaders, req.Header.Clone())
		path, ok := strings.CutPrefix(req.URL.Path, "/v1/files/")
		if !ok {
			http.NotFound(w, req)
			return
		}
		switch req.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(req.Body)
			e.files[path] = body
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			body, found := e.files[path]
			if !found {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	e.srv.Config.Protocols = new(http.Protocols)
	e.srv.Config.Protocols.SetHTTP1(true)
	if useTLS {
		e.srv.Config.Protocols.SetHTTP2(true)
		e.srv.EnableHTTP2 = true
		e.srv.StartTLS()
	} else {
		e.srv.Config.Protocols.SetUnencryptedHTTP2(true)
		e.srv.Start()
	}
	t.Cleanup(func() {
		e.srv.Close()
		grpcServer.Stop()
	})
	return e
}

func (e *attachEndpoint) lastREST(t *testing.T) http.Header {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.NotEmpty(t, e.restHeaders, "expected a REST request")
	return e.restHeaders[len(e.restHeaders)-1]
}

func (e *attachEndpoint) lastGRPC(t *testing.T) http.Header {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.NotEmpty(t, e.grpcHeaders, "expected a gRPC call")
	return e.grpcHeaders[len(e.grpcHeaders)-1]
}

// testTLS trusts the endpoint's certificate for both REST and gRPC.
func (e *attachEndpoint) testTLS() (*tls.Config, http.RoundTripper) {
	pool := x509.NewCertPool()
	pool.AddCert(e.srv.Certificate())
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, e.srv.Client().Transport
}

func TestAttachOverTLSSendsCallerHeadersOnRESTAndGRPC(t *testing.T) {
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{ExitCode: 0, Stdout: []byte("hello\n")}}
	e := startAttachEndpoint(t, true, svc)
	tlsConfig, transport := e.testTLS()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer platform-credential")
	headers.Set("X-Sandbox-ID", "sbx-123")
	sb, err := Attach(context.Background(), e.srv.URL, AttachOptions{
		Headers:       headers,
		TLSConfig:     tlsConfig,
		HTTPTransport: transport,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Close(context.Background()) })
	require.True(t, sb.IsReady())

	result, err := sb.Run(context.Background(), "echo hello")
	require.NoError(t, err)
	require.Equal(t, "hello\n", result.Stdout)
	got := e.lastGRPC(t)
	require.Equal(t, "Bearer platform-credential", got.Get("Authorization"))
	require.Equal(t, "sbx-123", got.Get("X-Sandbox-ID"))

	require.NoError(t, sb.Write(context.Background(), "notes.txt", []byte("hi")))
	content, err := sb.Read(context.Background(), "notes.txt")
	require.NoError(t, err)
	require.Equal(t, []byte("hi"), content)
	rest := e.lastREST(t)
	require.Equal(t, "Bearer platform-credential", rest.Get("Authorization"))
	require.Equal(t, "sbx-123", rest.Get("X-Sandbox-ID"))
	require.Empty(t, rest.Get(headerSandboxPort), "no routing headers without Routing")
}

func TestAttachWithRoutingSendsRouterHeadersPerProtocol(t *testing.T) {
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{}}
	e := startAttachEndpoint(t, false, svc)

	sb, err := Attach(context.Background(), e.srv.URL, AttachOptions{
		Routing: &AttachRouting{SandboxName: "my-sandbox", Namespace: "team-a"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Close(context.Background()) })

	_, err = sb.Run(context.Background(), "true")
	require.NoError(t, err)
	got := e.lastGRPC(t)
	require.Equal(t, "my-sandbox", got.Get(headerSandboxID))
	require.Equal(t, "team-a", got.Get(headerSandboxNamespace))
	require.Equal(t, "9090", got.Get(headerSandboxPort))

	require.NoError(t, sb.Write(context.Background(), "a.txt", []byte("x")))
	rest := e.lastREST(t)
	require.Equal(t, "my-sandbox", rest.Get(headerSandboxID))
	require.Equal(t, "team-a", rest.Get(headerSandboxNamespace))
	require.Equal(t, "8080", rest.Get(headerSandboxPort))
	require.Empty(t, rest.Get(headerSandboxPodIP))
	require.Equal(t, "my-sandbox", sb.SandboxName())
}

func TestAttachedHandleHasNoLifecycle(t *testing.T) {
	e := startAttachEndpoint(t, false, &fakeProcessService{response: &processv1.ExecuteResponse{}})
	sb, err := Attach(context.Background(), e.srv.URL, AttachOptions{})
	require.NoError(t, err)

	require.ErrorIs(t, sb.Suspend(context.Background()), ErrAttached)
	require.ErrorIs(t, sb.Resume(context.Background()), ErrAttached)
	_, err = sb.IsSuspended(context.Background())
	require.ErrorIs(t, err, ErrAttached)
	require.ErrorIs(t, sb.Open(context.Background()), ErrAlreadyOpen)

	require.NoError(t, sb.Close(context.Background()))
	require.False(t, sb.IsReady())
	require.ErrorIs(t, sb.Open(context.Background()), ErrAttached)
	_, err = sb.Run(context.Background(), "true")
	require.ErrorIs(t, err, ErrNotReady, "a closed attached handle refuses calls")
}

func TestAttachRejectsBadInput(t *testing.T) {
	for name, tc := range map[string]struct {
		url  string
		opts AttachOptions
	}{
		"no scheme":               {url: "sandbox.example.com"},
		"unsupported scheme":      {url: "ftp://sandbox.example.com"},
		"no host":                 {url: "https://"},
		"routing without sandbox": {url: "https://sandbox.example.com", opts: AttachOptions{Routing: &AttachRouting{}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Attach(context.Background(), tc.url, tc.opts)
			require.Error(t, err)
		})
	}
}

func TestAttachGRPCTarget(t *testing.T) {
	for in, want := range map[string]struct {
		target string
		tls    bool
	}{
		"https://sbx.example.com":          {"sbx.example.com:443", true},
		"https://sbx.example.com:8443/api": {"sbx.example.com:8443", true},
		"http://10.0.0.7":                  {"10.0.0.7:80", false},
		"http://[fd00::7]:8080":            {"[fd00::7]:8080", false},
	} {
		target, useTLS, err := attachGRPCTarget(in)
		require.NoError(t, err, in)
		require.Equal(t, want.target, target, in)
		require.Equal(t, want.tls, useTLS, in)
	}
}
