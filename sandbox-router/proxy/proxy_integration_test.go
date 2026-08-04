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
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

// newRouter builds a Handler that routes to backend via X-Sandbox-Pod-IP.
// The returned cleanup must be called by the test.
func newRouter(t *testing.T) *Handler {
	t.Helper()
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
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
	// Handler runs on the httptest server goroutine; the assertions
	// below run on the test goroutine after the client returns. Even
	// though that ordering is happens-after in wall-clock terms, the
	// race detector still requires explicit synchronization for the
	// shared accesses to be data-race-free.
	var (
		mu   sync.Mutex
		seen struct {
			method, path, query, host, sandboxID string
		}
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen.method = r.Method
		seen.path = r.URL.Path
		seen.query = r.URL.RawQuery
		seen.host = r.Host
		seen.sandboxID = r.Header.Get(HeaderSandboxID)
		mu.Unlock()
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
	mu.Lock()
	got := seen
	mu.Unlock()
	if got.method != http.MethodGet {
		t.Errorf("backend method: got %q", got.method)
	}
	if got.path != "/api/v1/items" {
		t.Errorf("backend path: got %q", got.path)
	}
	if got.query != "a=1&b=2" {
		t.Errorf("backend query: got %q", got.query)
	}
	if got.sandboxID != "test-sandbox" {
		t.Errorf("X-Sandbox-ID not forwarded; backend saw %q", got.sandboxID)
	}
	// httptest backend host = "127.0.0.1:NNNN" parsed from backend.URL — should
	// match the router-derived Host, not the original router.URL host. The
	// Python contract says we strip inbound Host so net/http picks URL host.
	wantHost := strings.TrimPrefix(backend.URL, "http://")
	if got.host != wantHost {
		t.Errorf("backend Host header: got %q want %q (inbound Host must be replaced)", got.host, wantHost)
	}
}

func TestIntegration_AllMethodsForwarded(t *testing.T) {
	var (
		mu         sync.Mutex
		methodSeen string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methodSeen = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
		mu.Lock()
		methodSeen = ""
		mu.Unlock()
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
		mu.Lock()
		got := methodSeen
		mu.Unlock()
		if got != m {
			t.Errorf("method %q forwarded as %q", m, got)
		}
	}
}

func TestIntegration_RequestBodyStreamed(t *testing.T) {
	// bytes.Buffer is not safe for concurrent access; the read in the
	// assertion below races the handler's Copy without a mutex.
	var (
		mu  sync.Mutex
		got bytes.Buffer
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_, _ = io.Copy(&got, r.Body)
		mu.Unlock()
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
	mu.Lock()
	gotStr, gotLen := got.String(), got.Len()
	mu.Unlock()
	if gotStr != payload {
		t.Errorf("body roundtrip mismatch: got %d bytes want %d", gotLen, len(payload))
	}
}

func TestIntegration_UpstreamConnectErrorReturns502(t *testing.T) {
	router := httptest.NewServer(newRouter(t))
	defer router.Close()

	// Point at a port that nothing is listening on — reserved-and-
	// closed so we're not assuming port 1 happens to be free on the
	// CI host.
	h := http.Header{}
	h.Set(HeaderSandboxID, "ghost")
	h.Set(HeaderSandboxNamespace, "test")
	h.Set(HeaderSandboxPodIP, "127.0.0.1")
	h.Set(HeaderSandboxPort, pickFreePortStr(t))

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
// GetByName scans entries so tests only have to populate one map. A
// mutex guards the maps/slices because the handler mutates them from
// the httptest server goroutine while assertions read from the test
// goroutine.
type stubLookup struct {
	mu                sync.Mutex
	entries           map[types.UID]cache.Entry
	invalidated       []types.UID
	invalidatedByName []string
}

func (s *stubLookup) Get(uid types.UID) (cache.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uid]
	return e, ok
}

func (s *stubLookup) GetByName(namespace, name string) (cache.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Namespace == namespace && e.SandboxName == name {
			return e, true
		}
	}
	return cache.Entry{}, false
}

func (s *stubLookup) Invalidate(uid types.UID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[uid]
	delete(s.entries, uid)
	s.invalidated = append(s.invalidated, uid)
	return ok
}

func (s *stubLookup) InvalidateByName(namespace, name, podIP string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidatedByName = append(s.invalidatedByName, namespace+"/"+name+"="+podIP)
	for uid, e := range s.entries {
		if e.Namespace == namespace && e.SandboxName == name && e.PodIP == podIP {
			delete(s.entries, uid)
			return true
		}
	}
	return false
}

// TestIntegration_CacheInvalidationOnDialError exercises the KEP-NNNN
// active invalidation: when the proxy dials a cached IP and the dial
// fails because the host is unreachable, the cache entry is evicted so
// the next request for the same UID falls through to DNS instead of
// retrying the stale IP.
func TestIntegration_CacheInvalidationOnDialError(t *testing.T) {
	cfg := config.Defaults()
	cfg.ProxyTimeout = time.Second // bounds the blackhole dial below
	cfg.ResponseHeaderTimeout = time.Second
	// Disable retries so a single dial failure shows up cleanly as one
	// upstream error and one invalidation.
	cfg.UpstreamMaxRetries = 0

	// 255.255.255.255 fails the dial instantly with ENETUNREACH — a
	// dead-host failure, unlike a refusal from a live host, which
	// intentionally does not evict (see the wrong-port test below).
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"sandbox-uid-xyz": {PodIP: "255.255.255.255", SandboxName: "s", Namespace: "ns"},
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

	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	req.Header.Set(HeaderSandboxID, "s")
	req.Header.Set(HeaderSandboxUID, "sandbox-uid-xyz")
	req.Header.Set(HeaderSandboxNamespace, "ns")
	req.Header.Set(HeaderSandboxPort, "8888")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}

	lookup.mu.Lock()
	defer lookup.mu.Unlock()
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
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
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
	req.Header.Set(HeaderSandboxPort, pickFreePortStr(t))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	if len(lookup.invalidated) != 0 || len(lookup.invalidatedByName) != 0 {
		t.Fatalf("expected no invalidations, got %v / %v", lookup.invalidated, lookup.invalidatedByName)
	}
}

// TestIntegration_NameIndexRoutesWithoutUID is the end-to-end regression
// for issue #883: a request carrying only X-Sandbox-Id + X-Sandbox-
// Namespace (what the SDKs send when they don't know the Pod IP) must
// route via the cache's name index instead of falling back to the DNS
// form — which for warm-pool sandboxes (no per-sandbox Service) can
// never resolve.
func TestIntegration_NameIndexRoutesWithoutUID(t *testing.T) {
	var (
		mu       sync.Mutex
		gotPath  string
		gotCalls int
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "warm-pool-ok")
	}))
	defer backend.Close()

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 5 * time.Second
	cfg.ResponseHeaderTimeout = 2 * time.Second

	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"warm-uid-1": {PodIP: u.Hostname(), SandboxName: "warm-sandbox", Namespace: "tenants"},
	}}

	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Cache:  lookup,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	// Only ID + namespace + port — no UID, no Pod-IP. Without the name
	// index this would try warm-sandbox.tenants.svc.cluster.local and 502.
	req, _ := http.NewRequest("GET", router.URL+"/execute", nil)
	req.Header.Set(HeaderSandboxID, "warm-sandbox")
	req.Header.Set(HeaderSandboxNamespace, "tenants")
	req.Header.Set(HeaderSandboxPort, u.Port())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %q)", resp.StatusCode, body)
	}
	if string(body) != "warm-pool-ok" {
		t.Errorf("body: got %q want warm-pool-ok", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotCalls != 1 || gotPath != "/execute" {
		t.Errorf("backend saw calls=%d path=%q, want 1 call to /execute", gotCalls, gotPath)
	}
}

// TestIntegration_CacheInvalidationOnNameDialError mirrors the UID-path
// active-invalidation test for name-resolved targets: a dead-host dial
// failure on an IP that came from the name index must evict that entry
// so the next request doesn't retry the stale IP.
func TestIntegration_CacheInvalidationOnNameDialError(t *testing.T) {
	cfg := config.Defaults()
	cfg.ProxyTimeout = time.Second // bounds the blackhole dial
	cfg.ResponseHeaderTimeout = time.Second
	cfg.UpstreamMaxRetries = 0

	// 255.255.255.255: instant ENETUNREACH (dead host).
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"warm-uid-1": {PodIP: "255.255.255.255", SandboxName: "warm-sandbox", Namespace: "tenants"},
	}}

	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Cache:  lookup,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	req.Header.Set(HeaderSandboxID, "warm-sandbox")
	req.Header.Set(HeaderSandboxNamespace, "tenants")
	req.Header.Set(HeaderSandboxPort, "8888")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	// The handler must pass the IP it actually dialed so the cache can
	// refuse the eviction if the entry was refreshed mid-dial.
	if len(lookup.invalidatedByName) != 1 || lookup.invalidatedByName[0] != "tenants/warm-sandbox=255.255.255.255" {
		t.Fatalf("expected one name invalidation for tenants/warm-sandbox=255.255.255.255, got %v", lookup.invalidatedByName)
	}
	if len(lookup.entries) != 0 {
		t.Fatalf("entry should have been removed from cache, still have %v", lookup.entries)
	}
}

// TestIntegration_NoEvictionOnConnectionRefused is the regression for the
// wrong-port review finding: a connection refusal proves a live host, so
// a caller-selected bad port (or a transiently down listener) must not
// evict the entry — for warm-pool sandboxes it is the only working route,
// and DNS can never take over. A follow-up request with the right port
// through the same name entry must still succeed.
func TestIntegration_NoEvictionOnConnectionRefused(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "still-routable")
	}))
	defer backend.Close()

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 2 * time.Second
	cfg.ResponseHeaderTimeout = time.Second
	cfg.UpstreamMaxRetries = 0

	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"warm-uid-1": {PodIP: u.Hostname(), SandboxName: "warm-sandbox", Namespace: "tenants"},
	}}

	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Cache:  lookup,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	send := func(port string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", router.URL+"/x", nil)
		req.Header.Set(HeaderSandboxID, "warm-sandbox")
		req.Header.Set(HeaderSandboxNamespace, "tenants")
		req.Header.Set(HeaderSandboxPort, port)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		return resp
	}

	// Wrong port: the live backend host refuses the connection. 502 to
	// the caller, but the entry must survive.
	resp := send(pickFreePortStr(t))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("wrong-port status: got %d want 502", resp.StatusCode)
	}
	lookup.mu.Lock()
	if len(lookup.invalidated) != 0 || len(lookup.invalidatedByName) != 0 {
		lookup.mu.Unlock()
		t.Fatalf("refusal must not evict, got %v / %v", lookup.invalidated, lookup.invalidatedByName)
	}
	lookup.mu.Unlock()

	// Right port through the same name entry still routes.
	resp = send(u.Port())
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "still-routable" {
		t.Fatalf("correct-port: got %d %q, want 200 still-routable", resp.StatusCode, body)
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

// TestIntegration_AuthorizationStrippedFromUpstream is the regression
// test for the privilege-escalation hazard the Python router avoids by
// dropping Authorization before forwarding. With --authz-mode=
// tokenreview the router consumes the caller's K8s bearer token; if
// that same token reached the sandbox, the sandbox could impersonate
// the caller against the K8s API or any other Bearer-protected
// service. The router must strip it.
func TestIntegration_AuthorizationStrippedFromUpstream(t *testing.T) {
	var (
		mu      sync.Mutex
		gotAuth = "<unset>"
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
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
	req.Header.Set("Authorization", "Bearer should-not-leak-to-upstream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	mu.Lock()
	got := gotAuth
	mu.Unlock()
	if got != "" {
		t.Fatalf("upstream saw Authorization=%q, want empty (router must strip)", got)
	}
}
