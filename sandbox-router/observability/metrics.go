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

// Package observability owns the sandbox-router's Prometheus collectors and
// the HTTP middleware that updates them.
package observability

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"sigs.k8s.io/agent-sandbox/internal/version"
)

// Metrics bundles every collector the router exposes. A Metrics value is
// constructed once at startup and registered into a private registry so
// tests do not collide with the global one and so we don't accidentally
// pick up unrelated controller-runtime metrics.
type Metrics struct {
	RequestsTotal           *prometheus.CounterVec
	RequestDurationSeconds  *prometheus.HistogramVec
	InflightRequests        prometheus.Gauge
	UpstreamErrorsTotal     *prometheus.CounterVec
	CertReloadsTotal        *prometheus.CounterVec
	UpstreamRetriesTotal    *prometheus.CounterVec
	CacheInvalidationsTotal *prometheus.CounterVec
	AuthzDecisionsTotal     *prometheus.CounterVec
	BuildInfo               prometheus.Collector
}

// NewMetrics creates a fresh set of collectors and registers them with reg.
// reg must be non-nil; pass a freshly-created prometheus.NewRegistry() to
// keep the router's series isolated.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_requests_total",
			Help: "Total HTTP requests handled by the router, labeled by method, status code, and target sandbox namespace.",
		}, []string{"method", "code", "sandbox_namespace"}),

		RequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sandbox_router_request_duration_seconds",
			Help:    "End-to-end request duration in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 180},
		}, []string{"method", "code", "sandbox_namespace"}),

		InflightRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sandbox_router_inflight_requests",
			Help: "Number of HTTP requests currently in flight.",
		}),

		UpstreamErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_upstream_errors_total",
			Help: "Errors connecting to the upstream sandbox, by namespace and reason.",
		}, []string{"sandbox_namespace", "reason"}),

		CertReloadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_cert_reloads_total",
			Help: "Server certificate reload attempts, labeled by outcome.",
		}, []string{"outcome"}),

		UpstreamRetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_upstream_retries_total",
			Help: "Upstream dial retries, labeled by namespace.",
		}, []string{"sandbox_namespace"}),

		CacheInvalidationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_cache_invalidations_total",
			Help: "Pod-IP cache entries evicted by the proxy after a dial-class failure on a cached IP (KEP-NNNN active invalidation).",
		}, []string{"sandbox_namespace"}),

		AuthzDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_authz_decisions_total",
			Help: "Per-request authorization outcomes, labeled by sandbox namespace and decision (allow / deny).",
		}, []string{"sandbox_namespace", "decision"}),

		BuildInfo: buildInfoCollector(),
	}

	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDurationSeconds,
		m.InflightRequests,
		m.UpstreamErrorsTotal,
		m.CertReloadsTotal,
		m.UpstreamRetriesTotal,
		m.CacheInvalidationsTotal,
		m.AuthzDecisionsTotal,
		m.BuildInfo,
	)
	return m
}

// buildInfoCollector mirrors internal/metrics.BuildInfo so a single shared
// dashboard can show both controller and router version info.
func buildInfoCollector() prometheus.Collector {
	v := version.Get()
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "sandbox_router_build_info",
			Help: "Sandbox router build metadata exposed as labels with a constant value of 1.",
			ConstLabels: prometheus.Labels{
				"git_version": v.GitVersion,
				"git_commit":  v.GitSHA,
				"build_date":  v.BuildDate,
				"go_version":  v.GoVersion,
				"compiler":    v.Compiler,
				"platform":    v.Platform,
			},
		},
		func() float64 { return 1 },
	)
}

// Middleware records inflight count, total requests, and request duration
// for every handled request. The sandbox_namespace label is read from the
// per-request *Labels attached to the request context — the proxy handler
// populates Labels.SandboxNamespace once header parsing succeeds.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.InflightRequests.Inc()
		defer m.InflightRequests.Dec()

		labels := &Labels{}
		ctx := WithLabels(r.Context(), labels)
		ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(ww, r.WithContext(ctx))
		dur := time.Since(start).Seconds()

		ns := labels.SandboxNamespace
		if ns == "" {
			ns = "-"
		}
		code := strconv.Itoa(ww.status)
		m.RequestsTotal.WithLabelValues(r.Method, code, ns).Inc()
		m.RequestDurationSeconds.WithLabelValues(r.Method, code, ns).Observe(dur)
	})
}

// statusRecorder captures the status code as it is written so the middleware
// can label metrics with it. WriteHeader is called at most once by stdlib
// semantics, and we mirror that.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write ensures that an implicit 200 from a body write is recorded.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying ResponseWriter if it supports it; this
// matters for streaming proxy responses driven by httputil.ReverseProxy with
// FlushInterval set.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter when it supports
// hijacking. Required for protocol upgrades — httputil.ReverseProxy
// type-asserts http.Hijacker on the ResponseWriter and bails out of
// the upgrade path if the assertion fails. Without this method, the
// metrics middleware silently breaks every WebSocket the router is
// supposed to carry. Returns http.ErrNotSupported when wrapping a
// ResponseWriter that itself doesn't support hijacking.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap exposes the underlying ResponseWriter for the stdlib's
// http.ResponseController helper. Go 1.20+ uses this to discover
// Flush/Hijack implementations under middleware wrappers.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
