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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/clients/go/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/clients/go/sandbox-router/observability"
)

// newRouter builds a Handler that routes to backend via X-Sandbox-Pod-IP.
// The returned cleanup must be called by the test.
func newRouter(t *testing.T) *Handler {
	t.Helper()
	cfg := config.Defaults()
	cfg.ProxyTimeout = 5 * time.Second
	cfg.ResponseHeaderTimeout = 2 * time.Second
	return NewHandler(Options{
		Config: &cfg,
		Logger: logr.Discard(),
	})
}

// podIPHeaders returns the headers that tell the router to forward directly
// to the given backend URL (parsed for host/port).
func podIPHeaders(t *testing.T, backendURL string) http.Header {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}
	h := http.Header{}
	h.Set(HeaderSandboxID, "test-sandbox")
	h.Set(HeaderSandboxNamespace, "test")
	h.Set(HeaderSandboxPodIP, u.Hostname())
	h.Set(HeaderSandboxPort, u.Port())
	return h
}

func TestIntegration_BasicProxyRoundTrip(t *testing.T) {
	gotMethod := ""
	gotPath := ""
	gotQuery := ""
	gotHost := ""
	gotXSandboxID := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHost = r.Host
		gotXSandboxID = r.Header.Get(HeaderSandboxID)
		w.Header().Set("X-From-Backend", "yes")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "backend-body")
	}))
	defer backend.Close()

	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		router.URL+"/api/v1/items?a=1&b=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range podIPHeaders(t, backend.URL) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 201 {
		t.Errorf("status: got %d want 201", resp.StatusCode)
	}
	if string(body) != "backend-body" {
		t.Errorf("body: got %q want backend-body", body)
	}
	if resp.Header.Get("X-From-Backend") != "yes" {
		t.Errorf("backend headers not forwarded")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("backend method: got %q", gotMethod)
	}
	if gotPath != "/api/v1/items" {
		t.Errorf("backend path: got %q", gotPath)
	}
	if gotQuery != "a=1&b=2" {
		t.Errorf("backend query: got %q", gotQuery)
	}
	if gotXSandboxID != "test-sandbox" {
		t.Errorf("X-Sandbox-ID not forwarded; backend saw %q", gotXSandboxID)
	}
	// httptest backend host = "127.0.0.1:NNNN" parsed from backend.URL — should
	// match the router-derived Host, not the original router.URL host. The
	// Python contract says we strip inbound Host so net/http picks URL host.
	wantHost := strings.TrimPrefix(backend.URL, "http://")
	if gotHost != wantHost {
		t.Errorf("backend Host header: got %q want %q (inbound Host must be replaced)", gotHost, wantHost)
	}
}

func TestIntegration_AllMethodsForwarded(t *testing.T) {
	methodSeen := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methodSeen = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
		methodSeen = ""
		req, _ := http.NewRequest(m, router.URL+"/", strings.NewReader("body"))
		for k, vs := range podIPHeaders(t, backend.URL) {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		resp.Body.Close()
		if methodSeen != m {
			t.Errorf("method %q forwarded as %q", m, methodSeen)
		}
	}
}

func TestIntegration_RequestBodyStreamed(t *testing.T) {
	var got bytes.Buffer
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(&got, r.Body)
		w.WriteHeader(200)
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	payload := strings.Repeat("payload-chunk-", 4096)
	req, _ := http.NewRequest("POST", router.URL+"/upload", strings.NewReader(payload))
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
	if got.String() != payload {
		t.Errorf("body roundtrip mismatch: got %d bytes want %d", got.Len(), len(payload))
	}
}

func TestIntegration_UpstreamConnectErrorReturns502(t *testing.T) {
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	// Point at a port that nothing is listening on.
	h := http.Header{}
	h.Set(HeaderSandboxID, "ghost")
	h.Set(HeaderSandboxNamespace, "test")
	h.Set(HeaderSandboxPodIP, "127.0.0.1")
	h.Set(HeaderSandboxPort, "1") // privileged port nothing listens on

	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ghost") {
		t.Errorf("body should mention sandbox id; got %q", body)
	}
	if !strings.HasPrefix(string(body), `{"detail":`) {
		t.Errorf("body should be JSON detail shape; got %q", body)
	}
}

// stubLookup is a minimal Lookup for integration tests of cache wiring.
type stubLookup struct {
	entries     map[types.UID]cache.Entry
	invalidated []types.UID
}

func (s *stubLookup) Get(uid types.UID) (cache.Entry, bool) {
	e, ok := s.entries[uid]
	return e, ok
}

func (s *stubLookup) Invalidate(uid types.UID) bool {
	_, ok := s.entries[uid]
	delete(s.entries, uid)
	s.invalidated = append(s.invalidated, uid)
	return ok
}

// TestIntegration_CacheInvalidationOnDialError exercises the KEP-NNNN
// active invalidation: when the proxy dials a cached IP and the dial
// fails, the cache entry is evicted so the next request for the same UID
// falls through to DNS instead of retrying the stale IP.
func TestIntegration_CacheInvalidationOnDialError(t *testing.T) {
	cfg := config.Defaults()
	cfg.ProxyTimeout = 2 * time.Second
	cfg.ResponseHeaderTimeout = 1 * time.Second
	// Disable retries so a single dial failure shows up cleanly as one
	// upstream error and one invalidation.
	cfg.UpstreamMaxRetries = 0

	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"sandbox-uid-xyz": {PodIP: "127.0.0.1", SandboxName: "s", Namespace: "ns"},
	}}

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	router := httptest.NewServer(NewHandler(Options{
		Config:  &cfg,
		Cache:   lookup,
		Metrics: metrics,
		Logger:  logr.Discard(),
	}))
	defer router.Close()

	// Dial 127.0.0.1:1 — nothing listens there, so the proxy will hit a
	// dial-class error and the ErrorHandler must invalidate the cache
	// entry for the UID we passed in.
	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	req.Header.Set(HeaderSandboxID, "s")
	req.Header.Set(HeaderSandboxUID, "sandbox-uid-xyz")
	req.Header.Set(HeaderSandboxNamespace, "ns")
	req.Header.Set(HeaderSandboxPort, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}

	if len(lookup.invalidated) != 1 || lookup.invalidated[0] != "sandbox-uid-xyz" {
		t.Fatalf("expected one invalidation for sandbox-uid-xyz, got %v", lookup.invalidated)
	}
	if _, still := lookup.entries["sandbox-uid-xyz"]; still {
		t.Fatalf("entry should have been removed from cache")
	}
}

// TestIntegration_NoInvalidationOnDNSDialError ensures we do NOT
// invalidate when the dial failure was on the DNS path — there is no
// cache entry to evict, and calling Invalidate would still trigger the
// metric, which would be misleading.
func TestIntegration_NoInvalidationOnDNSDialError(t *testing.T) {
	cfg := config.Defaults()
	cfg.ProxyTimeout = 2 * time.Second
	cfg.ResponseHeaderTimeout = 1 * time.Second
	cfg.UpstreamMaxRetries = 0

	lookup := &stubLookup{entries: map[types.UID]cache.Entry{}}

	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Cache:  lookup,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	// Pod-IP override means SourcePodIP, not SourceCache — invalidation
	// must not fire even with a UID present.
	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	req.Header.Set(HeaderSandboxID, "s")
	req.Header.Set(HeaderSandboxUID, "some-uid")
	req.Header.Set(HeaderSandboxNamespace, "ns")
	req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
	req.Header.Set(HeaderSandboxPort, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}
	if len(lookup.invalidated) != 0 {
		t.Fatalf("expected no invalidations, got %v", lookup.invalidated)
	}
}

func TestIntegration_MissingSandboxIDReturns400(t *testing.T) {
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	resp, err := http.Get(router.URL + "/any")
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
}
