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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestHealthzAlwaysOK(t *testing.T) {
	p := NewProbes()
	srv := httptest.NewServer(http.HandlerFunc(p.Healthz))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("body: got %q", body)
	}
}

func TestReadyzFlipsOnMarkUnready(t *testing.T) {
	p := NewProbes()
	p.MarkReady()
	srv := httptest.NewServer(http.HandlerFunc(p.Readyz))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready: got %d want 200", resp.StatusCode)
	}

	p.MarkUnready()
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unready: got %d want 503", resp.StatusCode)
	}
}

func TestReadyzDefaultsToUnready(t *testing.T) {
	p := NewProbes() // never marked ready
	srv := httptest.NewServer(http.HandlerFunc(p.Readyz))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("default unready: got %d want 503", resp.StatusCode)
	}
}

// TestStartProbes_HealthzAnswersBeforeReady covers the slow-startup case:
// the cache is still syncing, so readiness is false, but liveness must
// still return 200 or the kubelet restarts the pod.
func TestStartProbes_HealthzAnswersBeforeReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := NewProbes() // never marked ready
	stop, err := StartProbes(addr, p, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("StartProbes: %v", err)
	}
	defer stop()

	// Disable keep-alives so no idle client connection outlives the test
	// and trips goleak.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: got %d want 200", resp.StatusCode)
	}

	resp, err = client.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("readyz get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz: got %d want 503", resp.StatusCode)
	}
}

func TestStartProbes_BindFailureReturnsError(t *testing.T) {
	hog, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hog listen: %v", err)
	}
	defer hog.Close()

	if _, err := StartProbes(hog.Addr().String(), NewProbes(), logr.Discard(), nil); err == nil {
		t.Fatalf("expected bind error, got nil")
	}
}

// TestHealthz_FailsAfterStartupDeadline covers a hung startup step: liveness
// must turn red once the deadline passes, but only before the first ready.
func TestHealthz_FailsAfterStartupDeadline(t *testing.T) {
	p := NewProbes()
	p.SetStartupDeadline(time.Now().Add(-time.Second)) // already past

	rec := httptest.NewRecorder()
	p.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("past deadline, not ready: got %d want 503", rec.Code)
	}

	p.MarkReady()
	p.MarkUnready() // shutdown drain must not fail liveness
	rec = httptest.NewRecorder()
	p.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after ready then unready: got %d want 200", rec.Code)
	}
}

func TestMarkReadyIsOneTime(t *testing.T) {
	p := NewProbes()
	p.MarkReady()
	p.MarkUnready()
	p.MarkReady() // a drain must not be undone by a second MarkReady
	if p.isReady() {
		t.Fatal("MarkReady after MarkUnready made the pod ready again")
	}
}

func TestMarkUnreadyBeforeReadyKeepsStarting(t *testing.T) {
	p := NewProbes()
	p.MarkUnready()
	if p.isReady() {
		t.Fatal("MarkUnready before MarkReady reported ready")
	}
	p.MarkReady()
	if !p.isReady() {
		t.Fatal("MarkReady after an early MarkUnready did not become ready")
	}
}
