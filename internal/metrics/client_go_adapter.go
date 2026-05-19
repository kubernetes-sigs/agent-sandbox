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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

// Client-go metrics adapter wrapping prometheus metrics.

var (
	// HTTP request counter: tracks total requests by status code.
	// Use: rate(...{status_code=~"5.."}[5m]) / rate(...[5m]) for failure ratio.
	k8sClientHTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_k8s_client_http_requests_total",
			Help: "Total number of Kubernetes API client HTTP requests by status code.",
		},
		[]string{"status_code"},
	)

	// HTTP request duration histogram: tracks API latency by endpoint.
	// Use: histogram_quantile(0.99, rate(..._bucket[5m])) for P99 alerting.
	k8sClientHTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agent_sandbox_k8s_client_http_request_duration_seconds",
			Help:    "Kubernetes API client HTTP request duration in seconds by endpoint.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 2.0, 2.5, 5.0, 10.0, 30.0},
		},
		[]string{"endpoint"},
	)

	// Rate limiter duration histogram: tracks client-side throttling delay by endpoint.
	// Use: rate(..._sum[5m]) to detect frequent rate limiting.
	k8sClientRateLimiterDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agent_sandbox_k8s_client_rate_limiter_duration_seconds",
			Help:    "Kubernetes API client rate limiter wait duration in seconds by endpoint.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 2.0, 2.5, 5.0, 10.0, 30.0},
		},
		[]string{"endpoint"},
	)
)

// clientGoHTTPMetricAdapter implements client-go LatencyMetric and ResultMetric interfaces.
type clientGoHTTPMetricAdapter struct {
	count           *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	resultDelegate  clientmetrics.ResultMetric  // chained to preserve controller-runtime's rest_client_requests_total
	latencyDelegate clientmetrics.LatencyMetric // chained to preserve any pre-existing latency observers
}

// Increment implements metrics.ResultMetric.
func (a *clientGoHTTPMetricAdapter) Increment(ctx context.Context, code, method, host string) {
	a.count.WithLabelValues(code).Inc()
	if a.resultDelegate != nil {
		a.resultDelegate.Increment(ctx, code, method, host)
	}
}

// Observe implements metrics.LatencyMetric.
func (a *clientGoHTTPMetricAdapter) Observe(ctx context.Context, method string, u url.URL, d time.Duration) {
	a.duration.WithLabelValues(u.EscapedPath()).Observe(d.Seconds())
	if a.latencyDelegate != nil {
		a.latencyDelegate.Observe(ctx, method, u, d)
	}
}

// clientGoRateLimiterMetricAdapter implements client-go LatencyMetric for rate limiter delays.
type clientGoRateLimiterMetricAdapter struct {
	duration *prometheus.HistogramVec
	delegate clientmetrics.LatencyMetric // chained to preserve any pre-existing rate limiter observers
}

// Observe implements metrics.LatencyMetric.
func (a *clientGoRateLimiterMetricAdapter) Observe(ctx context.Context, method string, u url.URL, d time.Duration) {
	a.duration.WithLabelValues(u.EscapedPath()).Observe(d.Seconds())
	if a.delegate != nil {
		a.delegate.Observe(ctx, method, u, d)
	}
}

// Interface assertions.
var (
	_ clientmetrics.LatencyMetric = (*clientGoHTTPMetricAdapter)(nil)
	_ clientmetrics.ResultMetric  = (*clientGoHTTPMetricAdapter)(nil)
	_ clientmetrics.LatencyMetric = (*clientGoRateLimiterMetricAdapter)(nil)
)

// MustRegisterClientGoMetrics registers client-go metrics adapters with the global
// client-go metrics registry and the provided prometheus registerer.
func MustRegisterClientGoMetrics(registerer prometheus.Registerer) {
	// Capture existing global adapters so we can chain calls and preserve
	// any previously-installed metrics/instrumentation.
	existingResult := clientmetrics.RequestResult
	existingLatency := clientmetrics.RequestLatency
	existingRateLimiter := clientmetrics.RateLimiterLatency

	httpAdapter := &clientGoHTTPMetricAdapter{
		count:           k8sClientHTTPRequestsTotal,
		duration:        k8sClientHTTPRequestDuration,
		resultDelegate:  existingResult,
		latencyDelegate: existingLatency,
	}
	rateLimiterAdapter := &clientGoRateLimiterMetricAdapter{
		duration: k8sClientRateLimiterDuration,
		delegate: existingRateLimiter,
	}

	// Directly assign the global variables to work around controller-runtime's
	// single-registration limitation (clientmetrics.Register uses sync.Once).
	clientmetrics.RequestLatency = httpAdapter
	clientmetrics.RequestResult = httpAdapter
	clientmetrics.RateLimiterLatency = rateLimiterAdapter

	// Register prometheus metrics with the provided registerer.
	registerer.MustRegister(k8sClientHTTPRequestsTotal)
	registerer.MustRegister(k8sClientHTTPRequestDuration)
	registerer.MustRegister(k8sClientRateLimiterDuration)
}
