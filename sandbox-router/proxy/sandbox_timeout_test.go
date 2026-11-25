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
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

func TestParseSandboxTimeout(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		max     time.Duration
		want    time.Duration
		wantErr bool
	}{
		{name: "whole seconds", raw: "30", max: 180 * time.Second, want: 30 * time.Second},
		{name: "fractional seconds", raw: "0.5", max: 180 * time.Second, want: 500 * time.Millisecond},
		{name: "equal to cap", raw: "180", max: 180 * time.Second, want: 180 * time.Second},
		{name: "above cap", raw: "3600", max: 180 * time.Second, want: 180 * time.Second},
		{name: "huge value capped before overflow", raw: "1e300", max: 180 * time.Second, want: 180 * time.Second},
		{name: "zero", raw: "0", max: 180 * time.Second, wantErr: true},
		{name: "negative", raw: "-5", max: 180 * time.Second, wantErr: true},
		{name: "not a number", raw: "abc", max: 180 * time.Second, wantErr: true},
		{name: "empty", raw: "", max: 180 * time.Second, wantErr: true},
		{name: "NaN", raw: "NaN", max: 180 * time.Second, wantErr: true},
		{name: "infinity", raw: "Inf", max: 180 * time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSandboxTimeout(tt.raw, tt.max)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSandboxTimeout(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseSandboxTimeout(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// newTimeoutTestProxy returns a router in front of a backend that never
// answers, so the only way a request finishes is the router's deadline. The
// returned channel reports that the backend received the request, which
// distinguishes a canceled upstream request from a failure that never
// reached the backend.
func newTimeoutTestProxy(t *testing.T, proxyTimeout time.Duration) (routerURL, backendURL string, arrived <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(backend.Close)

	cfg := config.Defaults()
	cfg.ProxyTimeout = proxyTimeout
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.UpstreamMaxRetries = 0
	router := httptest.NewServer(NewHandler(Options{Config: &cfg, Logger: logr.Discard()}))
	t.Cleanup(router.Close)
	return router.URL, backend.URL, reached
}

// assertBackendReached fails when the router returned before forwarding the
// request, which would make an immediate 502 look like a timeout.
func assertBackendReached(t *testing.T, arrived <-chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(time.Second):
		t.Fatal("backend never received the request; the 502 came from a failure before the upstream deadline")
	}
}

// assertElapsedAtLeast rejects an immediate 502: the router must let the
// request run for a meaningful fraction of its budget before canceling.
func assertElapsedAtLeast(t *testing.T, elapsed, minimum time.Duration) {
	t.Helper()
	if elapsed < minimum {
		t.Fatalf("request ended after %s, want at least %s; the router failed before the deadline", elapsed, minimum)
	}
}

// requestThroughProxy sends one request with an optional X-Sandbox-Timeout
// header and returns how long the router took to give up.
func requestThroughProxy(t *testing.T, routerURL, backendURL, timeoutHeader string) time.Duration {
	t.Helper()
	backend, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, routerURL+"/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(HeaderSandboxID, "sandbox")
	req.Header.Set(HeaderSandboxNamespace, "ns")
	req.Header.Set(HeaderSandboxPodIP, backend.Hostname())
	req.Header.Set(HeaderSandboxPort, backend.Port())
	if timeoutHeader != "" {
		req.Header.Set(HeaderSandboxTimeout, timeoutHeader)
	}

	// A client-side cap keeps a regression that drops the deadline from
	// hanging the whole test binary.
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after the upstream deadline", resp.StatusCode)
	}
	return elapsed
}

func TestProxyHonorsSandboxTimeout(t *testing.T) {
	routerURL, backendURL, arrived := newTimeoutTestProxy(t, 5*time.Second)
	// Without the header this request would run for the full 5s.
	elapsed := requestThroughProxy(t, routerURL, backendURL, "0.2")
	assertBackendReached(t, arrived)
	assertElapsedAtLeast(t, elapsed, 100*time.Millisecond)
	if elapsed > 2*time.Second {
		t.Fatalf("request ran %s with X-Sandbox-Timeout: 0.2; the router ignored the header", elapsed)
	}
}

func TestProxyCapsSandboxTimeoutAtProxyTimeout(t *testing.T) {
	routerURL, backendURL, arrived := newTimeoutTestProxy(t, 300*time.Millisecond)
	// Honoring 60s outright would hang; the router must cap at 300ms.
	elapsed := requestThroughProxy(t, routerURL, backendURL, "60")
	assertBackendReached(t, arrived)
	assertElapsedAtLeast(t, elapsed, 100*time.Millisecond)
	if elapsed > 3*time.Second {
		t.Fatalf("request ran %s with X-Sandbox-Timeout: 60 and --proxy-timeout 300ms; the cap was not applied", elapsed)
	}
}

func TestProxyFallsBackOnInvalidSandboxTimeout(t *testing.T) {
	routerURL, backendURL, arrived := newTimeoutTestProxy(t, 300*time.Millisecond)
	elapsed := requestThroughProxy(t, routerURL, backendURL, "not-a-number")
	assertBackendReached(t, arrived)
	assertElapsedAtLeast(t, elapsed, 100*time.Millisecond)
	if elapsed > 3*time.Second {
		t.Fatalf("request ran %s with an invalid X-Sandbox-Timeout; the router did not fall back to --proxy-timeout", elapsed)
	}
}

func TestProxyUsesProxyTimeoutWithoutHeader(t *testing.T) {
	routerURL, backendURL, arrived := newTimeoutTestProxy(t, 300*time.Millisecond)
	elapsed := requestThroughProxy(t, routerURL, backendURL, "")
	assertBackendReached(t, arrived)
	assertElapsedAtLeast(t, elapsed, 100*time.Millisecond)
	if elapsed > 3*time.Second {
		t.Fatalf("request ran %s without X-Sandbox-Timeout; --proxy-timeout was not applied", elapsed)
	}
}
