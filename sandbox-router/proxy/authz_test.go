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
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// recordingAuthz lets a test pin the verdict for every call and inspect
// the canonical authorization target after the fact.
type recordingAuthz struct {
	mu       sync.Mutex
	err      error
	requests []recordedAuthzReq
}

type recordedAuthzReq struct {
	target authz.AuthorizationTarget
	hasTLS bool
	bearer string
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("parse integer %q: %v", value, err)
	}
	return parsed
}

func (a *recordingAuthz) Authorize(_ context.Context, r *http.Request, target authz.AuthorizationTarget) error {
	rec := recordedAuthzReq{target: target}
	if r != nil && r.TLS != nil {
		rec.hasTLS = true
	}
	if tok, ok := authz.BearerTokenFromRequest(r); ok {
		rec.bearer = tok
	}
	// Authorize is called on the httptest server goroutine; the test
	// reads `requests` after http.DefaultClient.Do returns. Even
	// though the read is happens-after the write in wall-clock
	// terms, Go's race detector requires explicit synchronization
	// for the access to be data-race-free.
	a.mu.Lock()
	a.requests = append(a.requests, rec)
	a.mu.Unlock()
	return a.err
}

// snapshot returns a copy of the recorded requests for tests to
// inspect without racing against an in-flight Authorize call.
func (a *recordingAuthz) snapshot() []recordedAuthzReq {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]recordedAuthzReq, len(a.requests))
	copy(out, a.requests)
	return out
}

func TestAuthzAllowedByDefault(t *testing.T) {
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	// No Authorizer set → AllowAll.
	h := NewHandler(Options{Config: &cfg, Logger: logr.Discard()})

	// Point at a dead port so we expect 502 — but importantly NOT 401/403.
	router := httptest.NewServer(h)
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	req.Header.Set(HeaderSandboxID, "s")
	req.Header.Set(HeaderSandboxNamespace, "ns")
	req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
	req.Header.Set(HeaderSandboxPort, pickFreePortStr(t)) // guaranteed-closed
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("AllowAll default should not 401/403; got %d", resp.StatusCode)
	}
}

func TestAuthzDenialMapsToStatus(t *testing.T) {
	cases := []struct {
		name       string
		denyErr    error
		wantStatus int
	}{
		{"unauthenticated → 401", authz.ErrUnauthenticated, http.StatusUnauthorized},
		{"forbidden → 403", authz.ErrForbidden, http.StatusForbidden},
		{"wrapped forbidden → 403", errors.Join(errors.New("ctx"), authz.ErrForbidden), http.StatusForbidden},
		{"unknown error → 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
			cfg.ProxyTimeout = 2 * time.Second
			cfg.UpstreamMaxRetries = 0
			a := &recordingAuthz{err: tc.denyErr}
			router := httptest.NewServer(NewHandler(Options{
				Config:     &cfg,
				Authorizer: a,
				Logger:     logr.Discard(),
			}))
			defer router.Close()

			req, _ := http.NewRequest("GET", router.URL+"/x", nil)
			req.Header.Set(HeaderSandboxID, "abc")
			req.Header.Set(HeaderSandboxNamespace, "team")
			// Pod-IP / port irrelevant — request must be rejected before
			// dialing — but use a real free port so a future regression
			// that lets the request through dial-fails instead of
			// hanging on whatever happens to be at port 1.
			req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
			req.Header.Set(HeaderSandboxPort, pickFreePortStr(t))
			req.Header.Set("Authorization", "Bearer test-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status: got %d want %d", resp.StatusCode, tc.wantStatus)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.HasPrefix(string(body), `{"detail":`) {
				t.Fatalf("body should be JSON detail shape; got %q", body)
			}
			calls := a.snapshot()
			if len(calls) != 1 {
				t.Fatalf("expected exactly one Authorize call, got %d", len(calls))
			}
			req0 := calls[0]
			if req0.target.Namespace != "team" || req0.target.SandboxName != "abc" {
				t.Fatalf("Authorize got (ns=%q, sandbox=%q), want (team, abc)", req0.target.Namespace, req0.target.SandboxName)
			}
			if req0.bearer != "test-token" {
				t.Fatalf("Authorize should see bearer token, got %q", req0.bearer)
			}
		})
	}
}

// TestAuthzRunsForPathRoutedRequests closes a gap that existed
// alongside --path-routing-prefix from its introduction: every
// existing authz test up to this one is header-routed, and every
// existing path-routing test uses the nil-Authorizer (AllowAll)
// default, so nothing exercised the combination — a path-routed
// request reaching Authorize with the (namespace, id) ParsePathRoute
// extracted from the URL, and a denial there mapping to the same HTTP
// status a header-routed denial would.
func TestAuthzRunsForPathRoutedRequests(t *testing.T) {
	cases := []struct {
		name       string
		denyErr    error
		wantStatus int
	}{
		{"allow", nil, http.StatusNoContent},
		{"unauthenticated → 401", authz.ErrUnauthenticated, http.StatusUnauthorized},
		{"forbidden → 403", authz.ErrForbidden, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			defer backend.Close()

			backendURL, err := url.Parse(backend.URL)
			if err != nil {
				t.Fatalf("parse backend URL: %v", err)
			}
			// A path-routed Target never carries a UID or PodIP override
			// (see ParsePathRoute) — it only becomes dialable here via
			// the namespace/name cache index, same as
			// TestPathRoutingPreservesEncodedSlash.
			lookup := &stubLookup{entries: map[types.UID]cache.Entry{
				"any-uid": {PodIP: backendURL.Hostname(), Namespace: "team-a", SandboxName: "sandbox-7"},
			}}

			cfg := config.Defaults()
			cfg.AllowLoopbackPodIP = true
			cfg.ProxyTimeout = 2 * time.Second
			cfg.UpstreamMaxRetries = 0
			cfg.PathRoutingPrefix = "/router"
			a := &recordingAuthz{err: tc.denyErr}
			router := httptest.NewServer(NewHandler(Options{
				Config:     &cfg,
				Cache:      lookup,
				Authorizer: a,
				Logger:     logr.Discard(),
			}))
			defer router.Close()

			resp, err := http.Get(router.URL + "/router/team-a/sandbox-7/" + backendURL.Port() + "/x%2Fy")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status: got %d want %d", resp.StatusCode, tc.wantStatus)
			}

			calls := a.snapshot()
			if len(calls) != 1 {
				t.Fatalf("expected exactly one Authorize call, got %d", len(calls))
			}
			if calls[0].target.Namespace != "team-a" || calls[0].target.SandboxName != "sandbox-7" {
				t.Fatalf("Authorize got (ns=%q, sandbox=%q) from the path, want (team-a, sandbox-7)", calls[0].target.Namespace, calls[0].target.SandboxName)
			}
			if calls[0].target.Port != mustAtoi(t, backendURL.Port()) || calls[0].target.Method != http.MethodGet || calls[0].target.Path != "/x%2Fy" {
				t.Fatalf("Authorize got target %+v, want routed port, GET, and /x%%2Fy", calls[0].target)
			}
		})
	}
}

func TestAuthzPassesNamespaceAndID(t *testing.T) {
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	a := &recordingAuthz{err: nil}
	router := httptest.NewServer(NewHandler(Options{
		Config:     &cfg,
		Authorizer: a,
		Logger:     logr.Discard(),
	}))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/x", nil)
	req.Header.Set(HeaderSandboxID, "sandbox-7")
	req.Header.Set(HeaderSandboxNamespace, "team-a")
	req.Header.Set(HeaderSandboxUID, "uid-7")
	req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
	port := pickFreePortStr(t)
	req.Header.Set(HeaderSandboxPort, port)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	calls := a.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected one Authorize call, got %d", len(calls))
	}
	if calls[0].target.Namespace != "team-a" || calls[0].target.SandboxName != "sandbox-7" {
		t.Fatalf("Authorize got (%q,%q) want (team-a, sandbox-7)", calls[0].target.Namespace, calls[0].target.SandboxName)
	}
	if calls[0].target.SandboxUID != "" || calls[0].target.Port != mustAtoi(t, port) || calls[0].target.Method != http.MethodGet || calls[0].target.Path != "/x" {
		t.Fatalf("Authorize got target %+v, want no cache-resolved UID, routed port, GET, and /x", calls[0].target)
	}
}

func TestScopedTokenV2BindsTheForwardedHeaderRoutedPath(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}

	tests := []struct {
		name            string
		requestPath     string
		requestRawPath  string
		tokenPath       string
		wantRequestPath string
	}{
		{
			name:            "literal percent",
			requestPath:     "/x%zz",
			requestRawPath:  "/x%25zz",
			tokenPath:       "/x%25zz",
			wantRequestPath: "/x%25zz",
		},
		{
			name:            "encoded slash follows header routing semantics",
			requestPath:     "/x/y",
			requestRawPath:  "/x%2Fy",
			tokenPath:       "/x/y",
			wantRequestPath: "/x/y",
		},
		{
			name:            "lowercase escapes become canonical",
			requestPath:     "/café",
			requestRawPath:  "/caf%c3%a9",
			tokenPath:       "/caf%C3%A9",
			wantRequestPath: "/caf%C3%A9",
		},
		{
			name:            "malformed raw escape becomes a literal percent",
			requestPath:     "/x%ZZ",
			requestRawPath:  "/x%ZZ",
			tokenPath:       "/x%25ZZ",
			wantRequestPath: "/x%25ZZ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.RequestURI
				w.WriteHeader(http.StatusNoContent)
			}))
			defer backend.Close()
			backendURL, err := url.Parse(backend.URL)
			if err != nil {
				t.Fatalf("parse backend URL: %v", err)
			}
			port := mustAtoi(t, backendURL.Port())

			token, err := authz.MintScopedTokenV2(privateKey, "current", authz.AuthorizationTarget{
				Namespace:   "team-a",
				SandboxName: "sandbox-7",
				SandboxUID:  "uid-7",
				Port:        port,
				Method:      http.MethodGet,
				Path:        test.tokenPath,
			}, time.Minute)
			if err != nil {
				t.Fatalf("mint v2 token: %v", err)
			}
			authorizer, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
				VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
			})
			if err != nil {
				t.Fatalf("new scoped-token authorizer: %v", err)
			}

			cfg := config.Defaults()
			cfg.AllowLoopbackPodIP = true
			cfg.ProxyTimeout = 2 * time.Second
			cfg.UpstreamMaxRetries = 0
			lookup := &stubLookup{entries: map[types.UID]cache.Entry{
				"uid-7": {
					PodIP:       backendURL.Hostname(),
					Namespace:   "team-a",
					SandboxName: "sandbox-7",
				},
			}}
			handler := NewHandler(Options{
				Config:     &cfg,
				Cache:      lookup,
				Authorizer: authorizer,
				Logger:     logr.Discard(),
			})

			req := httptest.NewRequest(http.MethodGet, "http://router.invalid/", nil)
			req.URL.Path = test.requestPath
			req.URL.RawPath = test.requestRawPath
			req.Header.Set(HeaderSandboxID, "sandbox-7")
			req.Header.Set(HeaderSandboxNamespace, "team-a")
			req.Header.Set(HeaderSandboxUID, "uid-7")
			req.Header.Set(HeaderSandboxPort, backendURL.Port())
			req.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			resp := response.Result()
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status: got %d want %d", resp.StatusCode, http.StatusNoContent)
			}
			if got := <-requests; got != test.wantRequestPath {
				t.Fatalf("upstream request path: got %q want %q", got, test.wantRequestPath)
			}
		})
	}
}

func TestScopedTokenV2RejectsNoncanonicalMethodBeforeForwarding(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	backendCalled := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalled <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	token, err := authz.MintScopedTokenV2(privateKey, "current", authz.AuthorizationTarget{
		Namespace:   "team-a",
		SandboxName: "sandbox-7",
		SandboxUID:  "uid-7",
		Port:        mustAtoi(t, backendURL.Port()),
		Method:      http.MethodPost,
		Path:        "/exec",
	}, time.Minute)
	if err != nil {
		t.Fatalf("mint v2 token: %v", err)
	}
	authorizer, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err != nil {
		t.Fatalf("new scoped-token authorizer: %v", err)
	}

	cfg := config.Defaults()
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"uid-7": {
			PodIP:       backendURL.Hostname(),
			Namespace:   "team-a",
			SandboxName: "sandbox-7",
		},
	}}
	router := httptest.NewServer(NewHandler(Options{
		Config:     &cfg,
		Cache:      lookup,
		Authorizer: authorizer,
		Logger:     logr.Discard(),
	}))
	defer router.Close()

	req, err := http.NewRequest("post", router.URL+"/exec", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(HeaderSandboxID, "sandbox-7")
	req.Header.Set(HeaderSandboxNamespace, "team-a")
	req.Header.Set(HeaderSandboxUID, "uid-7")
	req.Header.Set(HeaderSandboxPort, backendURL.Port())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d want %d", resp.StatusCode, http.StatusForbidden)
	}
	select {
	case <-backendCalled:
		t.Fatal("noncanonical method reached upstream")
	default:
	}
}
