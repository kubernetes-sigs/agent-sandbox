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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

// pickFreePort moved to helpers_test.go so non-integration tests
// (authz_test.go) can use it for "deterministically-dead destination"
// setup as well.

// startDelayedBackend brings up an HTTP listener on port after delay. It
// returns a stop func that the test must call to shut down the server. The
// returned channel closes once the listener is actually accepting.
func startDelayedBackend(t *testing.T, port int, delay time.Duration, handler http.Handler) (stop func(), ready <-chan struct{}) {
	t.Helper()
	readyCh := make(chan struct{})
	srv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(port),
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(delay)
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			// Port was taken between pickFreePort and now — race we can't
			// fully avoid. Surface the failure rather than hanging the test.
			t.Errorf("backend listen on :%d: %v", port, err)
			close(readyCh)
			return
		}
		close(readyCh)
		_ = srv.Serve(ln)
	}()
	stop = func() {
		_ = srv.Shutdown(context.Background())
		wg.Wait()
	}
	return stop, readyCh
}

// readCounter sums every series of a CounterVec into a single float so tests
// can assert "total retries > N" without caring about per-label breakdown.
func readCounter(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	var total float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("metric write: %v", err)
		}
		if pb.Counter != nil {
			total += pb.Counter.GetValue()
		}
	}
	return total
}

func TestIntegration_RetrySucceedsWhenBackendComesUp(t *testing.T) {
	port := pickFreePort(t)

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.UpstreamMaxRetries = 20
	cfg.UpstreamRetryInitialDelay = 20 * time.Millisecond
	cfg.UpstreamRetryMaxDelay = 100 * time.Millisecond
	cfg.ProxyTimeout = 5 * time.Second
	cfg.ResponseHeaderTimeout = 2 * time.Second

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	handler := NewHandler(Options{
		Config:  &cfg,
		Metrics: metrics,
		Logger:  logr.Discard(),
	})
	// Wrap with observability.Middleware so the namespace label is plumbed
	// into the per-request Labels struct (matches production wiring).
	router := httptest.NewServer(metrics.Middleware(handler))
	defer router.Close()

	// Backend goes live after enough time that at least a few retries have
	// fired. Delay > initial backoff + a couple of doublings.
	stop, ready := startDelayedBackend(t, port, 200*time.Millisecond,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "late-but-ready")
		}))
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, router.URL+"/anything", nil)
	req.Header.Set(HeaderSandboxID, "delayed-sandbox")
	req.Header.Set(HeaderSandboxNamespace, "default")
	req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
	req.Header.Set(HeaderSandboxPort, strconv.Itoa(port))

	// Cap the client timeout below the test deadline so a hang surfaces clearly.
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200; body=%q", resp.StatusCode, body)
	}
	if string(body) != "late-but-ready" {
		t.Errorf("body: got %q want late-but-ready", body)
	}

	// Confirm we actually exercised the retry path (rather than getting lucky
	// with timing). If the backend happened to come up before the first dial
	// the test still passes status-wise, but the assertion below would catch
	// a regression where retries are silently disabled.
	<-ready
	retries := readCounter(t, metrics.UpstreamRetriesTotal)
	if retries < 1 {
		t.Errorf("expected at least one retry given 200ms backend delay; metric=%v", retries)
	}
}

func TestIntegration_RetryGivesUpAndReturns502(t *testing.T) {
	// Point at a port nothing will ever listen on. With a small retry budget
	// the proxy should still return 502 within seconds.
	deadPort := pickFreePort(t)

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.UpstreamMaxRetries = 3
	cfg.UpstreamRetryInitialDelay = 10 * time.Millisecond
	cfg.UpstreamRetryMaxDelay = 50 * time.Millisecond
	cfg.ProxyTimeout = 3 * time.Second
	cfg.ResponseHeaderTimeout = 1 * time.Second

	handler := NewHandler(Options{Config: &cfg, Logger: logr.Discard()})
	router := httptest.NewServer(handler)
	defer router.Close()

	req, _ := http.NewRequest(http.MethodGet, router.URL+"/", nil)
	req.Header.Set(HeaderSandboxID, "ghost")
	req.Header.Set(HeaderSandboxNamespace, "default")
	req.Header.Set(HeaderSandboxPodIP, "127.0.0.1")
	req.Header.Set(HeaderSandboxPort, strconv.Itoa(deadPort))

	start := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ghost") {
		t.Errorf("body should mention sandbox id; got %q", body)
	}
	// Should take well under the full proxy timeout — bounded by the small
	// retry budget. 3 attempts with 10ms+20ms backoff ≈ 30ms plus dial RTTs.
	if elapsed > 2*time.Second {
		t.Errorf("gave up too slowly: %s", elapsed)
	}
}
