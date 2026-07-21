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

package metrics

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// controller-runtime's pkg/metrics only wires client-go's RequestResult hook
// (rest_client_requests_total): request COUNTS are exported but request
// LATENCY is not, so per-verb API round-trip time is invisible on the
// controller's /metrics endpoint. This file adds the two latency hooks,
// using the same metric names as k8s.io/component-base so standard
// dashboards and queries apply.

var (
	// restClientLatencyBuckets spans sub-ms cache hits through multi-second
	// throttled/contended calls (matches component-base's upper range while
	// adding finer low-end resolution for small, fast writes).
	restClientLatencyBuckets = []float64{
		0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
		1.0, 2.0, 4.0, 8.0, 15.0, 30.0, 60.0,
	}

	// requestLatency is the client-observed latency of Kubernetes API
	// requests, per verb and host. The URL path is deliberately dropped to
	// bound cardinality; resource-level splits come from the apiserver's own
	// apiserver_request_duration_seconds, and the delta between the two is
	// the network+client overhead this metric exists to expose.
	requestLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_request_duration_seconds",
			Help:    "Request latency in seconds. Broken down by verb and host.",
			Buckets: restClientLatencyBuckets,
		},
		[]string{"verb", "host"},
	)

	// rateLimiterLatency is the time requests spend waiting on the client-side
	// rate limiter before being sent, per verb and host. Non-zero values under
	// burst load mean --kube-api-qps/--kube-api-burst are the bottleneck, not
	// the apiserver.
	rateLimiterLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_rate_limiter_duration_seconds",
			Help:    "Client side rate limiter latency in seconds. Broken down by verb and host.",
			Buckets: restClientLatencyBuckets,
		},
		[]string{"verb", "host"},
	)
)

var registerRESTClientOnce sync.Once

// RegisterRESTClientLatencyMetrics registers the latency histograms with
// controller-runtime's global registry and installs the client-go latency
// hooks. It is called explicitly by the controller binary (not in init()) so
// that other binaries importing this package — e.g. sandbox-router, which
// serves a private Prometheus registry — don't pay for observations that are
// never exposed. Call it before any client issues requests; it is idempotent.
func RegisterRESTClientLatencyMetrics() {
	registerRESTClientOnce.Do(func() {
		metrics.Registry.MustRegister(requestLatency, rateLimiterLatency)

		// client-go's clientmetrics.Register is guarded by a sync.Once that
		// controller-runtime's pkg/metrics init() already consumes (wiring
		// only RequestResult), so a second Register call would be a silent
		// no-op. Instead, unconditionally overwrite the exported
		// RequestLatency/RateLimiterLatency hook variables — controller-runtime
		// leaves both at their no-op defaults, so nothing is lost, and doing
		// it before any client sends a request means no observation is missed.
		clientmetrics.RequestLatency = &latencyAdapter{metric: requestLatency}
		clientmetrics.RateLimiterLatency = &latencyAdapter{metric: rateLimiterLatency}
	})
}

type latencyAdapter struct {
	metric *prometheus.HistogramVec
}

func (l *latencyAdapter) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	l.metric.WithLabelValues(verb, u.Host).Observe(latency.Seconds())
}
