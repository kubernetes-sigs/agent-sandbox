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
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

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
