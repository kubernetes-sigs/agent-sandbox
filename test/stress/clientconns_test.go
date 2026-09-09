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
	"context"
	"encoding/pem"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/client-go/rest"
)

// connTrackingServer is an HTTP/2-enabled TLS test server that records which
// client connection (RemoteAddr) served each request. Mirrors the test rig
// for the controller's transport sharding (cmd/agent-sandbox-controller).
type connTrackingServer struct {
	srv *httptest.Server

	mu          sync.Mutex
	reqsPerConn map[string]int
}

func newConnTrackingServer(t *testing.T) *connTrackingServer {
	t.Helper()
	cts := &connTrackingServer{reqsPerConn: map[string]int{}}
	cts.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cts.mu.Lock()
		cts.reqsPerConn[r.RemoteAddr]++
		cts.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	cts.srv.EnableHTTP2 = true
	cts.srv.StartTLS()
	t.Cleanup(cts.srv.Close)
	return cts
}

func (c *connTrackingServer) restConfig() *rest.Config {
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw})
	return &rest.Config{
		Host:            c.srv.URL,
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}
}

func (c *connTrackingServer) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.reqsPerConn))
	maps.Copy(out, c.reqsPerConn)
	return out
}

// countingDialer wraps the standard dialer and counts TCP dials.
func countingDialer(dials *atomic.Int64) dialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

func TestConfigureCreateConnectionsDefaultIsNoop(t *testing.T) {
	cfg := &rest.Config{Host: "https://example.invalid"}
	if err := configureCreateConnections(cfg, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WrapTransport != nil {
		t.Error("connections=1 must not install a WrapTransport (historical single-connection behavior must be preserved)")
	}
	if cfg.Dial != nil {
		t.Error("connections=1 must not set a custom dialer")
	}
}

func TestConfigureCreateConnectionsRejectsInvalid(t *testing.T) {
	for _, n := range []int{0, -1} {
		if err := configureCreateConnections(&rest.Config{Host: "https://example.invalid"}, n); err == nil {
			t.Errorf("connections=%d: expected error, got nil", n)
		}
	}
}

func TestConfigureCreateConnectionsRejectsCustomDialOrTransport(t *testing.T) {
	cfg := &rest.Config{Host: "https://example.invalid"}
	cfg.Dial = (&net.Dialer{}).DialContext
	if err := configureCreateConnections(cfg, 2); err == nil {
		t.Error("expected error for config with custom Dial")
	}
	cfg2 := &rest.Config{Host: "https://example.invalid", Transport: http.DefaultTransport}
	if err := configureCreateConnections(cfg2, 2); err == nil {
		t.Error("expected error for config with custom Transport")
	}
}

// TestCreateShardingDistinctConnections verifies the core claim: with
// --client-connections=N, exactly N distinct TCP connections are dialed, all
// speak HTTP/2, and requests are distributed round-robin across them.
func TestCreateShardingDistinctConnections(t *testing.T) {
	const shardCount = 4
	const requests = 32

	server := newConnTrackingServer(t)
	cfg := server.restConfig()

	var dials atomic.Int64
	if err := configureCreateConnectionsWithDialer(cfg, shardCount, countingDialer(&dials)); err != nil {
		t.Fatalf("configureCreateConnectionsWithDialer: %v", err)
	}
	if cfg.Dial != nil {
		t.Fatal("sharding must not set Dial on the original config (only on shard copies)")
	}

	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("rest.HTTPClientFor: %v", err)
	}

	// Sequential requests: round-robin is deterministic and a shard never
	// dials twice (its connection is established by the prior request).
	for i := range requests {
		resp, err := httpClient.Get(fmt.Sprintf("%s/probe/%d", server.srv.URL, i))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("request %d: expected HTTP/2, got %s", i, resp.Proto)
		}
	}

	if got := dials.Load(); got != shardCount {
		t.Errorf("expected exactly %d TCP dials (one per shard), got %d", shardCount, got)
	}
	perConn := server.snapshot()
	if len(perConn) != shardCount {
		t.Errorf("expected %d distinct client connections at the server, got %d: %v", shardCount, len(perConn), perConn)
	}
	for addr, n := range perConn {
		if n != requests/shardCount {
			t.Errorf("connection %s served %d requests, expected exactly %d (round-robin)", addr, n, requests/shardCount)
		}
	}
}

// TestWatchClientKeepsOwnConnection verifies the create/watch split the
// harness relies on: a client built from the UNTOUCHED base config (the
// watch client) uses a connection distinct from every create shard, so
// create bursts cannot congest watch event delivery.
func TestWatchClientKeepsOwnConnection(t *testing.T) {
	const shardCount = 2

	server := newConnTrackingServer(t)
	baseCfg := server.restConfig()

	// Mirrors run(): the mutate config is a copy of the base; the base
	// config itself is never modified.
	mutateCfg := rest.CopyConfig(baseCfg)
	var shardDials atomic.Int64
	if err := configureCreateConnectionsWithDialer(mutateCfg, shardCount, countingDialer(&shardDials)); err != nil {
		t.Fatalf("configureCreateConnectionsWithDialer: %v", err)
	}
	mutateClient, err := rest.HTTPClientFor(mutateCfg)
	if err != nil {
		t.Fatalf("rest.HTTPClientFor(mutate): %v", err)
	}

	for i := range 2 * shardCount {
		resp, err := mutateClient.Get(server.srv.URL + "/create")
		if err != nil {
			t.Fatalf("mutate request %d: %v", i, err)
		}
		resp.Body.Close()
	}
	createConns := server.snapshot()
	if len(createConns) != shardCount {
		t.Fatalf("expected %d create-shard connections, got %d: %v", shardCount, len(createConns), createConns)
	}

	watchClient, err := rest.HTTPClientFor(baseCfg)
	if err != nil {
		t.Fatalf("rest.HTTPClientFor(base): %v", err)
	}
	for i := range 3 {
		resp, err := watchClient.Get(server.srv.URL + "/watch")
		if err != nil {
			t.Fatalf("watch request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("watch request %d: expected HTTP/2, got %s", i, resp.Proto)
		}
	}

	allConns := server.snapshot()
	if len(allConns) != shardCount+1 {
		t.Errorf("expected %d total connections (create shards + dedicated watch), got %d: %v", shardCount+1, len(allConns), allConns)
	}
	watchConns := 0
	for addr := range allConns {
		if _, usedByCreates := createConns[addr]; !usedByCreates {
			watchConns++
		}
	}
	if watchConns != 1 {
		t.Errorf("expected exactly 1 connection exclusive to the watch client, got %d", watchConns)
	}
}
