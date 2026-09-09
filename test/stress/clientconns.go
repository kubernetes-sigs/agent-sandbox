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

// configureCreateConnections optionally shards the harness's MUTATING API
// traffic (claim/sandbox/pool creates and deletes) across `connections`
// independent TCP+TLS+HTTP/2 connections.
//
// Why: the kube-apiserver advertises SETTINGS_MAX_CONCURRENT_STREAMS=100 by
// default, and client-go multiplexes every request built from one rest.Config
// onto a single HTTP/2 connection. Historically the stress tool put ALL of
// its traffic — e.g. 300 simultaneous claim creates during the claims-warm
// burst, PLUS the five resource watches, PLUS metrics scrapes — on that one
// connection. Under a 300-wide create burst, creates queue in the harness
// waiting for one of ~100 streams, which inflates the measured create-ack
// (and therefore create→Ready) with client-side queueing that says nothing
// about the controller or the apiserver. This is the exact ceiling the
// controller hit and fixed in cmd/agent-sandbox-controller/transport.go; this
// file mirrors that approach for the load generator.
//
// How: identical mechanism to the controller's configureAPIConnections — for
// each shard, copy the rest.Config and set a distinct Dial function.
// client-go's TLS transport cache keys on the resulting DialHolder pointer,
// so every shard gets its own *http.Transport and its own TCP/HTTP2
// connection, using only supported client-go API. A round-robin RoundTripper
// over the shards is installed via cfg.WrapTransport.
//
// Callers apply this to a COPY of the base rest.Config and build the
// mutating dynamic client from it; the watch/scrape clients keep the
// untouched base config. Because the base config has no custom Dial, its
// transport cache entry is distinct from every shard's, so with
// connections > 1 the watch streams always ride a connection that create
// bursts cannot congest.
//
// connections == 1 leaves cfg completely untouched: one connection shared by
// creates, watches, and scrapes — the historical stress-tool behavior.
func configureCreateConnections(cfg *rest.Config, connections int) error {
	return configureCreateConnectionsWithDialer(cfg, connections, nil)
}

// configureCreateConnectionsWithDialer is configureCreateConnections with an
// injectable base dialer so tests can count/observe the underlying TCP
// connections. A nil baseDial uses a per-shard net.Dialer identical to
// client-go's default (30s timeout, 30s keep-alive).
func configureCreateConnectionsWithDialer(cfg *rest.Config, connections int, baseDial dialFunc) error {
	if connections < 1 {
		return fmt.Errorf("client-connections must be >= 1, got %d", connections)
	}
	if connections == 1 {
		// Default: preserve historical single-connection behavior exactly.
		return nil
	}
	if cfg.Dial != nil || cfg.Transport != nil {
		return fmt.Errorf("client-connections > 1 is incompatible with a custom Dial/Transport on the rest.Config")
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
			return fmt.Errorf("building create-path connection shard %d/%d: %w", i+1, connections, err)
		}
		shards = append(shards, rt)
	}

	sharded := &shardedRoundTripper{shards: shards}
	cfg.WrapTransport = func(http.RoundTripper) http.RoundTripper {
		return sharded
	}
	return nil
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
