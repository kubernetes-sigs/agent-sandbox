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

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// freeAddr returns a loopback address with a port nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestRun_HealthzAnswersDuringSlowCacheSync pins the fix: while the initial
// pod LIST is still pending, /healthz must return 200 (so the kubelet does not
// restart the pod) and /readyz must return 503.
func TestRun_HealthzAnswersDuringSlowCacheSync(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "slow", http.StatusInternalServerError)
	}))
	defer api.Close()
	// Registered after api.Close, so it runs first: unblocks the pending LIST
	// so run() returns before the test server shuts down.
	defer close(release)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`, api.URL)
	if err := os.WriteFile(kubeconfig, []byte(kc), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	probeAddr := freeAddr(t)
	cfg := config.Defaults()
	cfg.HTTPAddr = freeAddr(t)
	cfg.MetricsAddr = freeAddr(t)
	cfg.HTTPSAddr = ""
	cfg.ProbeAddr = probeAddr
	cfg.CacheEnabled = true
	cfg.CacheNamespace = "default"
	cfg.Kubeconfig = kubeconfig

	runErr := make(chan error, 1)
	go func() { runErr <- run(&cfg, logr.Discard()) }()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	get := func(path string) (int, error) {
		resp, err := client.Get("http://" + probeAddr + path)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}

	// Wait for the probe listener to come up, then check both probes while
	// the sync is still blocked.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-runErr:
			t.Fatalf("run() returned before the probes were checked: %v", err)
		default:
		}
		code, err := get("/healthz")
		if err == nil {
			if code != http.StatusOK {
				t.Fatalf("healthz during sync: got %d want 200", code)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe listener never came up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	code, err := get("/readyz")
	if err != nil {
		t.Fatalf("readyz get: %v", err)
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz during sync: got %d want 503", code)
	}
}
