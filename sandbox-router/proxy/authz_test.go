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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// recordingAuthz lets a test pin the verdict for every call and inspect
// the (ns, sandbox) arguments after the fact.
type recordingAuthz struct {
	mu       sync.Mutex
	err      error
	requests []recordedAuthzReq
}

type recordedAuthzReq struct {
	ns      string
	sandbox string
	hasTLS  bool
	bearer  string
}

func (a *recordingAuthz) Authorize(_ context.Context, r *http.Request, ns, name string) error {
	rec := recordedAuthzReq{ns: ns, sandbox: name}
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
			if req0.ns != "team" || req0.sandbox != "abc" {
				t.Fatalf("Authorize got (ns=%q, sandbox=%q), want (team, abc)", req0.ns, req0.sandbox)
			}
			if req0.bearer != "test-token" {
				t.Fatalf("Authorize should see bearer token, got %q", req0.bearer)
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
	req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
	req.Header.Set(HeaderSandboxPort, pickFreePortStr(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	calls := a.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected one Authorize call, got %d", len(calls))
	}
	if calls[0].ns != "team-a" || calls[0].sandbox != "sandbox-7" {
		t.Fatalf("Authorize got (%q,%q) want (team-a, sandbox-7)", calls[0].ns, calls[0].sandbox)
	}
}
