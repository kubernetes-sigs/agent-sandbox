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

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// newH2CBackend serves plain-text HTTP/2 so the router's gRPC transport
// (h2c prior knowledge) can dial it. gRPC-Web requests share that
// transport after classification, so every backend in these tests must
// speak h2c even though the payloads are hand-built.
func newH2CBackend(t *testing.T, next http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(next)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	return srv
}

// grpcWebCORSServer builds a router handler pointed at a capturing backend
// via X-Sandbox-Pod-IP, with one allowed cross-origin entry.
func grpcWebCORSServer(t *testing.T, a authz.Authorizer) (*Handler, *http.Request) {
	t.Helper()
	backend := newH2CBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.UpstreamMaxRetries = 0
	cfg.GRPCWebAllowedOrigins = []string{"https://app.example.com"}
	h := NewHandler(Options{Config: &cfg, Authorizer: a, Logger: logr.Discard()})
	t.Cleanup(h.Close)

	// Rewrite the backend URL into the Pod-IP header form the router dials.
	addr := strings.TrimPrefix(backend.URL, "http://")
	host, port, _ := strings.Cut(addr, ":")
	r := httptest.NewRequest(http.MethodPost, "/spec.process.v1.ProcessService/Execute", strings.NewReader("\x00\x00\x00\x00\x00"))
	r.Header.Set("Content-Type", contentTypeGRPCWeb)
	r.Header.Set("X-Sandbox-ID", "sb1")
	r.Header.Set("X-Sandbox-Port", port)
	r.Header.Set("X-Sandbox-Pod-IP", host)
	return h, r
}

type denyAuthorizer struct{}

func (denyAuthorizer) Authorize(context.Context, *http.Request, authz.AuthorizationTarget) error {
	return authz.ErrForbidden
}

func TestPreflight_AllowedOrigin(t *testing.T) {
	h, _ := grpcWebCORSServer(t, nil)
	r := httptest.NewRequest(http.MethodOptions, "/spec.process.v1.ProcessService/Execute", nil)
	r.Header.Set("Origin", "https://app.example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "content-type,grpc-timeout,x-sandbox-id")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q, want echoed origin (never *)", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC = %q, want true", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("Allow-Methods = %q, want it to include POST", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "content-type,grpc-timeout,x-sandbox-id" {
		t.Fatalf("Allow-Headers = %q", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("Max-Age = %q, want 600", got)
	}
	if got := w.Header().Values("Vary"); len(got) == 0 || got[0] != "Origin" {
		t.Fatalf("Vary = %v, want [Origin]", got)
	}
}

func TestPreflight_UnlistedOriginFallsThrough(t *testing.T) {
	// Only explicitly allowlisted origins are intercepted. Browsers
	// preflight cross-origin requests only, so answering a preflight
	// from any other origin would change the proxying behavior of
	// existing non-gRPC-Web browser traffic without helping any caller.
	// The fall-through proof: the request reaches routing and fails on
	// the missing X-Sandbox-ID, exactly as before this feature existed.
	h, _ := grpcWebCORSServer(t, nil)
	r := httptest.NewRequest(http.MethodOptions, "/svc/Method", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from the routing fall-through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "X-Sandbox-ID header is required") {
		t.Fatalf("body = %q, want the routing error, not a preflight answer", w.Body.String())
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("fall-through preflight must not gain CORS headers")
	}
}

func TestPreflight_SameOriginFallsThrough(t *testing.T) {
	// Same-origin fetches never trigger a CORS preflight in a browser,
	// so intercepting same-origin OPTIONS helps nobody and would divert
	// same-origin REST preflights away from sandboxes that answer them.
	h, _ := grpcWebCORSServer(t, nil)
	r := httptest.NewRequest(http.MethodOptions, "/svc/Method", nil)
	r.Header.Set("Origin", "http://example.com")
	r.Host = "example.com"
	r.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from the routing fall-through", w.Code)
	}
}

func TestPreflight_HeaderIntersection(t *testing.T) {
	// Headers outside the gRPC-Web set are never echoed back, which is
	// how CORS stays scoped to gRPC-Web traffic: a plain-REST preflight
	// asking for anything else gets no usable Allow-Headers and the
	// browser blocks the real request.
	h, _ := grpcWebCORSServer(t, nil)
	r := httptest.NewRequest(http.MethodOptions, "/svc/Method", nil)
	r.Header.Set("Origin", "https://app.example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "x-sandbox-id, x-evil-header, grpc-timeout, x-grpc-web")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	got := w.Header().Get("Access-Control-Allow-Headers")
	if strings.Contains(got, "x-evil-header") {
		t.Fatalf("Allow-Headers leaked an unknown header: %q", got)
	}
	for _, want := range []string{"x-sandbox-id", "grpc-timeout", "x-grpc-web"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Allow-Headers = %q, missing %q", got, want)
		}
	}
}

func TestNonPreflightOPTIONS_FallsThrough(t *testing.T) {
	// An OPTIONS without the preflight markers is a normal proxied
	// request — existing behavior must not change.
	var sawOptions bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawOptions = r.Method == http.MethodOptions
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.UpstreamMaxRetries = 0
	h := NewHandler(Options{Config: &cfg, Logger: logr.Discard()})
	defer h.Close()

	addr := strings.TrimPrefix(backend.URL, "http://")
	host, port, _ := strings.Cut(addr, ":")
	r := httptest.NewRequest(http.MethodOptions, "/some/path", nil)
	r.Header.Set("X-Sandbox-ID", "sb1")
	r.Header.Set("X-Sandbox-Port", port)
	r.Header.Set("X-Sandbox-Pod-IP", host)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if !sawOptions {
		t.Fatal("non-preflight OPTIONS was not proxied to the backend")
	}
}

func TestRealRequest_CORSHeadersOnAuthzDeny(t *testing.T) {
	// The browser must be able to READ the denial, so CORS headers go on
	// before authorization runs.
	h, r := grpcWebCORSServer(t, denyAuthorizer{})
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK { // grpc-web errors are 200 + status frame
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "grpc-status:7") {
		t.Fatalf("body = %q, want PermissionDenied frame", w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q on denied response, want echoed origin", got)
	}
}

func TestRealRequest_DisallowedOriginProxiedWithoutCORS(t *testing.T) {
	// Standard CORS semantics: the server still executes the request
	// (non-browser clients send Origin too); the browser blocks the read
	// because no ACAO comes back.
	h, r := grpcWebCORSServer(t, nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want none for a disallowed origin", got)
	}
}

func TestOutboundContentTypeRewrite(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"application/grpc-web", "application/grpc"},
		{"application/grpc-web+proto", "application/grpc+proto"},
		// classifyRequest parses media-type parameters away, so the
		// rewrite must too: forwarding the raw value would make the
		// backend reject the RPC with 415 (reviewer aditya-shantanu).
		{"application/grpc-web; charset=utf-8", "application/grpc"},
		{"application/grpc-web+proto;charset=utf-8", "application/grpc+proto"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			var gotCT string
			backend := newH2CBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCT = r.Header.Get("Content-Type")
				w.Header().Set("Content-Type", "application/grpc")
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()
			cfg := config.Defaults()
			cfg.AllowLoopbackPodIP = true
			cfg.UpstreamMaxRetries = 0
			h := NewHandler(Options{Config: &cfg, Logger: logr.Discard()})
			defer h.Close()

			addr := strings.TrimPrefix(backend.URL, "http://")
			host, port, _ := strings.Cut(addr, ":")
			r := httptest.NewRequest(http.MethodPost, "/svc/Method", strings.NewReader("\x00\x00\x00\x00\x00"))
			r.Header.Set("Content-Type", tc.in)
			r.Header.Set("X-Sandbox-ID", "sb1")
			r.Header.Set("X-Sandbox-Port", port)
			r.Header.Set("X-Sandbox-Pod-IP", host)
			w := httptest.NewRecorder()

			h.ServeHTTP(w, r)

			if gotCT != tc.want {
				t.Fatalf("backend saw Content-Type %q, want %q", gotCT, tc.want)
			}
		})
	}
}

func TestNativeGRPC_NoCORSHeadersAdded(t *testing.T) {
	h, _ := grpcWebCORSServer(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/svc/Method", strings.NewReader("\x00\x00\x00\x00\x00"))
	r.ProtoMajor = 2
	r.Header.Set("Content-Type", "application/grpc")
	r.Header.Set("X-Sandbox-ID", "sb1")
	r.Header.Set("X-Sandbox-Port", "1") // dial fails; we only assert response headers
	r.Header.Set("X-Sandbox-Pod-IP", "127.0.0.1")
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("native gRPC response gained ACAO %q; CORS is gRPC-Web only", got)
	}
}

func TestRejectedVariant_AllowedOriginGetsCORSHeaders(t *testing.T) {
	// The reject path must stay readable from the browser: without CORS
	// headers the InvalidArgument frame is an opaque CORS error and the
	// router's explanation never reaches the caller.
	h, r := grpcWebCORSServer(t, nil)
	r.Header.Set("Content-Type", "application/grpc-web-text")
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "grpc-status:3") {
		t.Fatalf("body = %q, want InvalidArgument frame", w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q on rejected-variant response, want echoed origin", got)
	}
}
