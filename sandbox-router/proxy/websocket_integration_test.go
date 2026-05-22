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
