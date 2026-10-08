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

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestRun_DrainsHTTP2RequestBeforeCanceling(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	release := make(chan struct{})
	probes := NewProbes()
	srv, err := New(Options{
		Log: logr.Discard(), Probes: probes, HTTPAddr: addr, ShutdownTimeout: 3 * time.Second,
		ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-release:
				_, _ = io.WriteString(w, "completed during drain")
			case <-r.Context().Done():
				_, _ = io.WriteString(w, "canceled before grace expired")
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(cancel)
	deadline := time.Now().Add(2 * time.Second)
	for !probes.ready.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !probes.ready.Load() {
		t.Fatal("router never ready")
	}
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	deadline = time.Now().Add(time.Second)
	for probes.ready.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if probes.ready.Load() {
		t.Fatal("readiness stayed true during shutdown")
	}
	close(release)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "completed during drain" {
		t.Fatalf("drained body=%q err=%v", body, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
}

func TestRun_ForcedShutdownCancelsActiveRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ended := make(chan struct{})
	probes := NewProbes()
	srv, err := New(Options{
		Log: logr.Discard(), Probes: probes, HTTPAddr: addr, ShutdownTimeout: 50 * time.Millisecond,
		ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(ended)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		w := httptest.NewRecorder()
		probes.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("router not ready")
		}
		time.Sleep(time.Millisecond)
	}
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("active upstream context survived forced shutdown")
	}
}

// TestRun_BindFailureSurfacesSynchronously verifies that Run() returns an
// error if a listener can't bind, rather than crashing in a background
// goroutine after MarkReady() has already flipped /readyz to 200.
//
// We grab a port, hold it, then try to start the server on the same port.
// Pre-bind should fail; Run() should return the error and never advertise
// readiness.
func TestRun_BindFailureSurfacesSynchronously(t *testing.T) {
	hog, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hog listen: %v", err)
	}
	defer hog.Close()
	occupiedAddr := hog.Addr().String()

	probes := NewProbes()
	srv, err := New(Options{
		Log:             logr.Discard(),
		Probes:          probes,
		ProxyHandler:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		HTTPAddr:        occupiedAddr, // will collide with `hog`
		ShutdownTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := srv.Run(context.Background())
	if runErr == nil {
		t.Fatalf("expected bind error from Run(), got nil")
	}
	if !strings.Contains(runErr.Error(), "listen") {
		t.Errorf("expected listen-related error, got: %v", runErr)
	}
	// Critical assertion: readiness must NOT have been flipped to true when
	// startup failed — otherwise a freshly-launched pod would briefly tell
	// the LB it's ready while the proxy port is unreachable.
	if probes.ready.Load() {
		t.Errorf("readiness must remain false when bind fails")
	}
}

func TestRun_PlainHTTP1AndHTTP2(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	probes := NewProbes()
	srv, err := New(Options{
		Log: logr.Discard(), Probes: probes, HTTPAddr: addr, ShutdownTimeout: time.Second,
		ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Protocol", r.Proto)
			w.WriteHeader(http.StatusOK)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		w := httptest.NewRecorder()
		probes.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("router never ready")
		}
		time.Sleep(time.Millisecond)
	}
	for _, protocol := range []string{"HTTP/1.1", "HTTP/2.0"} {
		t.Run(protocol, func(t *testing.T) {
			tr := &http.Transport{Protocols: new(http.Protocols)}
			tr.Protocols.SetHTTP1(protocol == "HTTP/1.1")
			tr.Protocols.SetUnencryptedHTTP2(protocol == "HTTP/2.0")
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: time.Second}
			resp, err := client.Get("http://" + addr + "/test")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("X-Protocol"); got != protocol {
				t.Fatalf("protocol = %s, want %s", got, protocol)
			}
		})
	}
}

// TestRun_CleansUpPriorBindsOnLaterBindFailure ensures we don't leak a
// half-bound state when listener N+1 fails to bind. Closing the prior
// listeners is the only way to release the ports for a retry.
func TestRun_CleansUpPriorBindsOnLaterBindFailure(t *testing.T) {
	// Reserve one address as the future "collision" target.
	hog, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hog listen: %v", err)
	}
	defer hog.Close()
	collidingAddr := hog.Addr().String()

	// Pick a free port for the HTTP listener so it binds successfully
	// before the metrics bind fails.
	freeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free listen: %v", err)
	}
	freeAddr := freeLn.Addr().String()
	_ = freeLn.Close()

	probes := NewProbes()
	srv, err := New(Options{
		Log:             logr.Discard(),
		Probes:          probes,
		ProxyHandler:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		MetricsHandler:  http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		HTTPAddr:        freeAddr,      // binds OK
		MetricsAddr:     collidingAddr, // collides — bind fails
		ShutdownTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if runErr := srv.Run(context.Background()); runErr == nil {
		t.Fatalf("expected error from Run()")
	}

	// The previously-bound HTTP port must have been released; we should
	// be able to bind it again.
	retry, err := net.Listen("tcp", freeAddr)
	if err != nil {
		t.Fatalf("port %s was not released after partial bind failure: %v", freeAddr, err)
	}
	_ = retry.Close()
}

// TestRun_ReadyOnlyAfterAllBindsSucceed exercises the happy path: start the
// server, wait for /readyz to flip true, confirm it's true, then cancel.
func TestRun_ReadyOnlyAfterAllBindsSucceed(t *testing.T) {
	probes := NewProbes()
	srv, err := New(Options{
		Log:             logr.Discard(),
		Probes:          probes,
		ProxyHandler:    http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
		HTTPAddr:        "127.0.0.1:0",
		ShutdownTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if probes.ready.Load() {
		t.Fatalf("ready should start false")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Poll briefly; Run() flips ready synchronously after pre-bind, so
	// this loop should succeed on the first or second tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if probes.ready.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !probes.ready.Load() {
		cancel()
		<-done
		t.Fatalf("ready never flipped to true")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
	if probes.ready.Load() {
		t.Errorf("ready should be false after shutdown")
	}
}
