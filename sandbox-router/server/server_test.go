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
	"crypto/tls"
	"crypto/x509"
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
	addr := unusedTCPAddr(t)
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
	cancel, wait := runTestServer(t, srv)
	waitForReadiness(t, probes, true)
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
	waitForReadiness(t, probes, false)
	close(release)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "completed during drain" {
		t.Fatalf("drained body=%q err=%v", body, err)
	}
	if err := wait(); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
}

func TestRun_ForcedShutdownCancelsActiveRequest(t *testing.T) {
	addr := unusedTCPAddr(t)
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
	cancel, wait := runTestServer(t, srv)
	waitForReadiness(t, probes, true)
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
	if err := wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("active upstream context survived forced shutdown")
	}
}

func TestRun_ForcedShutdownDoesNotWaitForBlockedHandler(t *testing.T) {
	addr := unusedTCPAddr(t)
	release := make(chan struct{})
	ended := make(chan struct{})
	probes := NewProbes()
	srv, err := New(Options{
		Log: logr.Discard(), Probes: probes, HTTPAddr: addr, ShutdownTimeout: 50 * time.Millisecond,
		ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			defer close(ended)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			// A stuck handler must not turn forced shutdown into an
			// unbounded wait for all request goroutines.
			<-release
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel, wait := runTestServer(t, srv)
	waitForReadiness(t, probes, true)
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/stream")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	defer func() {
		close(release)
		awaitCompletion(t, ended, "blocked handler")
	}()
	cancel()
	if err := wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced shutdown while handler remains blocked: %v", err)
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
	addr := unusedTCPAddr(t)
	probes := NewProbes()
	srv, err := New(Options{
		Log: logr.Discard(), Probes: probes, HTTPAddr: addr, ShutdownTimeout: time.Second,
		ProxyHandler: http.HandlerFunc(echoProtocol),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel, wait := runTestServer(t, srv)
	t.Cleanup(func() {
		cancel()
		if err := wait(); err != nil {
			t.Error(err)
		}
	})
	waitForReadiness(t, probes, true)
	assertListenerProtocols(t, "http://"+addr, nil)
}

func TestRun_TLSHTTP1AndHTTP2(t *testing.T) {
	certFixture := httptest.NewTLSServer(http.NotFoundHandler())
	certFixture.Close()
	cert := &certFixture.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certFixture.Certificate())
	addr := unusedTCPAddr(t)
	probes := NewProbes()
	srv, err := New(Options{
		Log: logr.Discard(), Probes: probes, HTTPSAddr: addr, ShutdownTimeout: time.Second,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil },
		},
		ProxyHandler: http.HandlerFunc(echoProtocol),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel, wait := runTestServer(t, srv)
	t.Cleanup(func() {
		cancel()
		if err := wait(); err != nil {
			t.Error(err)
		}
	})
	waitForReadiness(t, probes, true)
	assertListenerProtocols(t, "https://"+addr, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
}

func echoProtocol(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Protocol", r.Proto)
	w.WriteHeader(http.StatusOK)
}

func assertListenerProtocols(t *testing.T, baseURL string, tlsConfig *tls.Config) {
	t.Helper()
	for _, protocol := range []string{"HTTP/1.1", "HTTP/2.0"} {
		t.Run(protocol, func(t *testing.T) {
			tr := &http.Transport{Protocols: new(http.Protocols), TLSClientConfig: tlsConfig}
			tr.Protocols.SetHTTP1(protocol == "HTTP/1.1")
			tr.Protocols.SetUnencryptedHTTP2(protocol == "HTTP/2.0" && tlsConfig == nil)
			tr.Protocols.SetHTTP2(protocol == "HTTP/2.0" && tlsConfig != nil)
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: time.Second}
			resp, err := client.Get(baseURL + "/test")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("X-Protocol"); got != protocol || resp.Proto != protocol {
				t.Fatalf("backend protocol = %s, wire protocol = %s, want %s", got, resp.Proto, protocol)
			}
		})
	}
}

func unusedTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitForReadiness(t *testing.T, probes *Probes, ready bool) {
	t.Helper()
	want := http.StatusServiceUnavailable
	if ready {
		want = http.StatusOK
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		w := httptest.NewRecorder()
		probes.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness = %d, want %d", w.Code, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func runTestServer(t *testing.T, srv *Server) (context.CancelFunc, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = srv.Run(ctx)
		close(done)
	}()
	wait := func() error {
		t.Helper()
		select {
		case <-done:
			return runErr
		case <-time.After(5 * time.Second):
			t.Fatal("router server goroutine did not stop")
			return nil
		}
	}
	t.Cleanup(func() { cancel(); _ = wait() })
	return cancel, wait
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
