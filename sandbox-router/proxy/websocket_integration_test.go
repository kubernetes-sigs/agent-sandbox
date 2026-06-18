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

//go:build integration

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// echoUpgrader is a permissive WebSocket upgrader for tests — origin
// checks live in front-of-router auth, not in the router itself.
var echoUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// startEchoBackend stands up an httptest server whose root endpoint
// upgrades to WebSocket and echoes every text frame back.
func startEchoBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := echoUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, payload); err != nil {
				return
			}
		}
	}))
}

// dialThroughRouter opens a WebSocket connection to routerURL (the
// router's http base) with the sandbox routing headers pointed at
// backendURL.
func dialThroughRouter(t *testing.T, routerURL, backendURL string) *websocket.Conn {
	t.Helper()
	bu, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}
	// ws:// scheme so gorilla/websocket sends the right Upgrade headers.
	wsURL := strings.Replace(routerURL, "http://", "ws://", 1) + "/"
	hdrs := http.Header{}
	hdrs.Set(HeaderSandboxID, "ws-sandbox")
	hdrs.Set(HeaderSandboxNamespace, "test")
	hdrs.Set(HeaderSandboxPodIP, bu.Hostname())
	hdrs.Set(HeaderSandboxPort, bu.Port())

	dialer := websocket.DefaultDialer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := dialer.DialContext(ctx, wsURL, hdrs)
	if err != nil {
		status := -1
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("ws dial: %v (status=%d)", err, status)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("ws handshake: got %d, want 101", resp.StatusCode)
	}
	return conn
}

// TestIntegration_WebSocketUpgradeRoundTrips proves that
// httputil.ReverseProxy's built-in Upgrade handling survives our
// wrapping (Rewrite callback, transport, ErrorHandler) and that text
// frames bounce off a real backend.
func TestIntegration_WebSocketUpgradeRoundTrips(t *testing.T) {
	backend := startEchoBackend(t)
	defer backend.Close()

	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	conn := dialThroughRouter(t, router.URL, backend.URL)
	defer conn.Close()

	for _, msg := range []string{"hello", "world", "from-router"} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, got, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != msg {
			t.Fatalf("echo: got %q want %q", got, msg)
		}
	}
}

// TestIntegration_WebSocketOutlivesProxyTimeout is the regression test
// for the comment on #838: ProxyTimeout must NOT apply once the
// connection has been upgraded. Without the fix in proxy.go's
// ServeHTTP, the context.WithTimeout(ctx, ProxyTimeout) tears the
// connection down at the timeout mark (180s default — would surface
// as code-server WebSocket close 1006).
//
// We set ProxyTimeout to a value SHORTER than how long we keep the
// connection idle. If the timeout applies, the read will fail mid-test.
func TestIntegration_WebSocketOutlivesProxyTimeout(t *testing.T) {
	backend := startEchoBackend(t)
	defer backend.Close()

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true             // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 500 * time.Millisecond // deliberately tiny
	cfg.ResponseHeaderTimeout = 2 * time.Second
	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	conn := dialThroughRouter(t, router.URL, backend.URL)
	defer conn.Close()

	// Idle for ~3x the ProxyTimeout. A naive WithTimeout(ProxyTimeout)
	// wrapper would have killed the upgraded connection by now.
	time.Sleep(1500 * time.Millisecond)

	// After the idle, the connection must still ferry frames in both
	// directions. SetReadDeadline guards the test from hanging on a
	// failure mode where the connection is half-closed.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte("post-timeout")); err != nil {
		t.Fatalf("write after sleep: %v", err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read after sleep: %v (this means ProxyTimeout is killing upgraded connections)", err)
	}
	if string(got) != "post-timeout" {
		t.Fatalf("echo: got %q want post-timeout", got)
	}
}

// TestIntegration_NonUpgradeStillRespectsProxyTimeout makes sure the
// upgrade carve-out didn't accidentally disable the timeout for normal
// requests. A slow backend that holds the response past ProxyTimeout
// must still be cut off with 502.
func TestIntegration_NonUpgradeStillRespectsProxyTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the response longer than the router's ProxyTimeout.
		select {
		case <-time.After(3 * time.Second):
			w.WriteHeader(200)
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 300 * time.Millisecond
	cfg.ResponseHeaderTimeout = 5 * time.Second // don't let this fire first
	cfg.UpstreamMaxRetries = 0
	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/slow", nil)
	for k, vs := range podIPHeaders(t, backend.URL) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502 (ProxyTimeout should have fired)", resp.StatusCode)
	}
	// Sanity: we should have failed near the timeout, not near the
	// backend's 3s hold.
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("ProxyTimeout did not bound the request: %s elapsed", elapsed)
	}
}

// TestIntegration_WebSocketStripsOriginOnUpgrade locks in the
// vscode-server compatibility fix. The router rewrites Host to the
// upstream sandbox's address, so a client-supplied Origin that
// matches the router's external hostname would no longer match what
// the backend sees as its own Host. CSRF-aware backends (vscode-
// server, Jupyter) reject the WebSocket upgrade with 1006 Close on
// that mismatch. We drop Origin on upgrade requests so the backend
// sees "no Origin assertion" rather than a bad one.
func TestIntegration_WebSocketStripsOriginOnUpgrade(t *testing.T) {
	var (
		mu        sync.Mutex
		gotOrigin = "<unset>"
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotOrigin = r.Header.Get("Origin")
		mu.Unlock()
		conn, err := echoUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend upgrade: %v", err)
			return
		}
		conn.Close()
	}))
	defer backend.Close()

	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	// Build the dial ourselves so we can set Origin explicitly — the
	// dialThroughRouter helper doesn't expose it.
	bu, _ := url.Parse(backend.URL)
	wsURL := strings.Replace(router.URL, "http://", "ws://", 1) + "/"
	hdrs := http.Header{}
	hdrs.Set(HeaderSandboxID, "ws-sandbox")
	hdrs.Set(HeaderSandboxNamespace, "test")
	hdrs.Set(HeaderSandboxPodIP, bu.Hostname())
	hdrs.Set(HeaderSandboxPort, bu.Port())
	hdrs.Set("Origin", "https://router.example.com")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, hdrs)
	if err != nil {
		t.Fatalf("ws dial: %v (status=%v)", err, resp)
	}
	conn.Close()

	mu.Lock()
	got := gotOrigin
	mu.Unlock()
	if got != "" {
		t.Fatalf("backend saw Origin=%q, want empty (router must strip on upgrade)", got)
	}
}

// TestIntegration_NonUpgradePreservesOrigin guards the converse:
// regular (non-Upgrade) HTTP requests must NOT lose Origin, because
// stripping it would break CORS preflights and any backend that uses
// Origin for legitimate same-origin checks on non-WebSocket traffic.
func TestIntegration_NonUpgradePreservesOrigin(t *testing.T) {
	var (
		mu        sync.Mutex
		gotOrigin = "<unset>"
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotOrigin = r.Header.Get("Origin")
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/", nil)
	for k, vs := range podIPHeaders(t, backend.URL) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Origin", "https://client.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	mu.Lock()
	got := gotOrigin
	mu.Unlock()
	if got != "https://client.example.com" {
		t.Fatalf("backend Origin: got %q want https://client.example.com (only strip on upgrade)", got)
	}
}

// TestIntegration_XForwardedHeadersSet exercises pr.SetXForwarded():
// the upstream sandbox needs to know the client-visible Host and
// scheme to construct correct self-links / redirects (especially
// for browser-facing backends like Jupyter or vscode-server). For
// plain HTTP we should see Proto=http; X-Forwarded-For should
// carry the inbound client address.
func TestIntegration_XForwardedHeadersSet(t *testing.T) {
	var (
		mu                        sync.Mutex
		gotHost, gotProto, gotFor string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Header.Get("X-Forwarded-Host")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotFor = r.Header.Get("X-Forwarded-For")
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/", nil)
	for k, vs := range podIPHeaders(t, backend.URL) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	host, proto, fwdFor := gotHost, gotProto, gotFor
	mu.Unlock()

	wantHost := strings.TrimPrefix(router.URL, "http://")
	if host != wantHost {
		t.Errorf("X-Forwarded-Host: got %q want %q", host, wantHost)
	}
	if proto != "http" {
		t.Errorf("X-Forwarded-Proto: got %q want %q", proto, "http")
	}
	if fwdFor == "" {
		t.Errorf("X-Forwarded-For: empty; expected the client address")
	}
}
