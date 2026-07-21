// Copyright 2025 The Kubernetes Authors.
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
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
)

// dialFunc matches rest.Config.Dial / net.Dialer.DialContext.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// configureAPIConnections optionally shards the controller's Kubernetes API
// traffic across `connections` independent TCP+TLS+HTTP/2 connections.
//
// Why: the kube-apiserver caps concurrent streams per HTTP/2 connection
// (SETTINGS_MAX_CONCURRENT_STREAMS). The server-side limit comes from
// --http2-max-streams-per-connection, which defaults to 0 = the Go HTTP/2
// server default of 250; managed control planes can advertise less (the GKE
// clusters we benchmark advertise 100). client-go multiplexes all requests
// from one rest.Config onto a single HTTP/2 connection, so that advertised
// limit caps effective in-flight API requests regardless of worker counts or
// client-side QPS settings. Sharding across N connections (each dialed
// lazily on first use) raises the ceiling to ~N times the per-connection
// limit.
//
// How: for each shard we copy the rest.Config and set a distinct Dial
// function. rest.Config.TransportConfig wraps Dial in a fresh
// *transport.DialHolder per call, and client-go's TLS transport cache keys on
// that pointer, so every shard is guaranteed its own *http.Transport and
// therefore its own TCP/HTTP2 connection — using only supported client-go
// API. The shards are full transports built by rest.TransportFor (TLS + auth
// wrappers included; the outer wrapper chain's bearer/user-agent round
// trippers are no-ops when the inner chain already set the headers, and vice
// versa). A round-robin RoundTripper distributing over the shards is then
// installed via cfg.WrapTransport, which every client built from this config
// (manager cache/watches, manager client, event recorder, leader election)
// picks up. All shards remain HTTP/2, so watches keep HTTP/2 semantics.
//
// connections == 1 leaves cfg completely untouched (stock single-connection
// behavior).
func configureAPIConnections(cfg *rest.Config, connections int) error {
	return configureAPIConnectionsWithDialer(cfg, connections, nil)
}

// configureAPIConnectionsWithDialer is configureAPIConnections with an
// injectable base dialer so tests can count/observe the underlying TCP
// connections. A nil baseDial uses a per-shard net.Dialer identical to
// client-go's default (30s timeout, 30s keep-alive).
func configureAPIConnectionsWithDialer(cfg *rest.Config, connections int, baseDial dialFunc) error {
	if connections < 1 {
		return fmt.Errorf("api-connections must be >= 1, got %d", connections)
	}
	if connections == 1 {
		// Default: preserve current behavior exactly; do not touch cfg.
		return nil
	}
	if cfg.Dial != nil || cfg.Transport != nil {
		return fmt.Errorf("api-connections > 1 is incompatible with a custom Dial/Transport on the rest.Config")
	}
	if cfg.WrapTransport != nil {
		// Installing the shard router would silently replace a pre-set
		// wrapper (tracing, instrumentation, ...); refuse instead so the
		// incompatibility is explicit, mirroring the Dial/Transport check.
		return fmt.Errorf("api-connections > 1 is incompatible with a pre-set WrapTransport on the rest.Config")
	}

	shards := make([]http.RoundTripper, 0, connections)
	for i := range connections {
		shardCfg := rest.CopyConfig(cfg)
		// The shard transports live *below* the WrapTransport slot of the
		// outer config; never inherit an outer wrapper (recursion guard).
		shardCfg.WrapTransport = nil
		dial := baseDial
		if dial == nil {
			// Mirrors the default dialer client-go's transport cache uses.
			dial = (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext
		}
		// Setting Dial forces a distinct transport cache entry per shard
		// (fresh DialHolder pointer per TransportFor call).
		shardCfg.Dial = dial
		rt, err := rest.TransportFor(shardCfg)
		if err != nil {
			return fmt.Errorf("building API connection shard %d/%d: %w", i+1, connections, err)
		}
		shards = append(shards, rt)
	}

	sharded := &shardedRoundTripper{shards: shards}
	// rest.HTTPClientFor is called several times with this config
	// (manager, cluster, leader election, ...); always hand back the same
	// shared shard set so the process uses exactly `connections`
	// connections in total. The base transport client-go built for this
	// config is intentionally unused.
	cfg.WrapTransport = func(http.RoundTripper) http.RoundTripper {
		return sharded
	}
	return nil
}

// newIsolatedHTTPClient returns an *http.Client for cfg that is guaranteed
// its own dedicated TCP+TLS+HTTP/2 connection, never shared with (or wrapped
// by) any other client built from cfg. Used to give the manager cache's
// list/watch streams (manager.Options.Cache.HTTPClient) a connection that
// write bursts cannot congest: watch event delivery otherwise stalls behind
// hundreds of concurrent write streams competing for the same connection's
// SETTINGS_MAX_CONCURRENT_STREAMS budget.
//
// Call this BEFORE configureAPIConnections installs its WrapTransport on cfg
// (the copy also defensively clears WrapTransport). The distinct Dial forces
// a distinct client-go transport cache entry, exactly as in
// configureAPIConnectionsWithDialer.
func newIsolatedHTTPClient(cfg *rest.Config) (*http.Client, error) {
	return newIsolatedHTTPClientWithDialer(cfg, nil)
}

// newIsolatedHTTPClientWithDialer is newIsolatedHTTPClient with an injectable
// base dialer for tests.
func newIsolatedHTTPClientWithDialer(cfg *rest.Config, baseDial dialFunc) (*http.Client, error) {
	isoCfg := rest.CopyConfig(cfg)
	isoCfg.WrapTransport = nil
	dial := baseDial
	if dial == nil {
		dial = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	isoCfg.Dial = dial
	return rest.HTTPClientFor(isoCfg)
}

// shardedRoundTripper distributes requests round-robin over a fixed set of
// independent transports (one HTTP/2 connection each).
type shardedRoundTripper struct {
	shards []http.RoundTripper
	next   atomic.Uint64
}

func (s *shardedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	idx := s.next.Add(1) % uint64(len(s.shards))
	return s.shards[idx].RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the
// per-shard transports (net/http checks for this interface).
func (s *shardedRoundTripper) CloseIdleConnections() {
	type closeIdler interface{ CloseIdleConnections() }
	for _, shard := range s.shards {
		rt := shard
		for rt != nil {
			if ci, ok := rt.(closeIdler); ok {
				ci.CloseIdleConnections()
				break
			}
			wrapper, ok := rt.(utilnet.RoundTripperWrapper)
			if !ok {
				break
			}
			rt = wrapper.WrappedRoundTripper()
		}
	}
}
