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
	"io"
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
	// ClientRxBytesTotal counts bytes received from clients (request bodies).
	ClientRxBytesTotal *prometheus.CounterVec
	// ClientTxBytesTotal counts bytes sent to clients (response bodies).
	ClientTxBytesTotal *prometheus.CounterVec
	// UpstreamRxBytesTotal counts bytes read from upstream sandbox backends.
	UpstreamRxBytesTotal *prometheus.CounterVec
	// UpstreamTxBytesTotal counts bytes written to upstream sandbox backends.
	UpstreamTxBytesTotal *prometheus.CounterVec
	// RequestSizeBytes observes request body size distributions.
	RequestSizeBytes *prometheus.HistogramVec
	// ResponseSizeBytes observes response body size distributions. SSE
	// streaming responses can be very large.
	ResponseSizeBytes *prometheus.HistogramVec
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

		ClientRxBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_client_rx_bytes_total",
			Help: "Total bytes received from clients (request bodies), labeled by sandbox namespace.",
		}, []string{"sandbox_namespace"}),

		ClientTxBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_client_tx_bytes_total",
			Help: "Total bytes sent to clients (response bodies), labeled by sandbox namespace.",
		}, []string{"sandbox_namespace"}),

		UpstreamRxBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_upstream_rx_bytes_total",
			Help: "Total bytes read from upstream sandbox backends, labeled by sandbox namespace.",
		}, []string{"sandbox_namespace"}),

		UpstreamTxBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandbox_router_upstream_tx_bytes_total",
			Help: "Total bytes written to upstream sandbox backends, labeled by sandbox namespace.",
		}, []string{"sandbox_namespace"}),

		RequestSizeBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sandbox_router_request_size_bytes",
			Help:    "Distribution of request body sizes in bytes, labeled by sandbox namespace.",
			Buckets: []float64{100, 1024, 10240, 102400, 1048576, 10485760, 104857600, 1073741824},
		}, []string{"sandbox_namespace"}),

		ResponseSizeBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sandbox_router_response_size_bytes",
			Help:    "Distribution of response body sizes in bytes, labeled by sandbox namespace. SSE streaming responses may be very large.",
			Buckets: []float64{100, 1024, 10240, 102400, 1048576, 10485760, 104857600, 1073741824},
		}, []string{"sandbox_namespace"}),

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
		m.ClientRxBytesTotal,
		m.ClientTxBytesTotal,
		m.UpstreamRxBytesTotal,
		m.UpstreamTxBytesTotal,
		m.RequestSizeBytes,
		m.ResponseSizeBytes,
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
// per-request *Labels attached to the request context via LabelsForRequest
// — the proxy handler populates Labels.SandboxNamespace once routing
// resolves, regardless of whether that came from headers or (with
// path-based routing enabled) the URL path. LabelsForRequest reuses an
// outer middleware's Labels when one is already attached (as
// TracingMiddleware's is, in the real chain) rather than shadowing it with
// a second, disconnected instance the proxy handler would never see —
// see its doc comment — and falls back to allocating its own when none is
// attached, so this middleware still works correctly wired up standalone.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.InflightRequests.Inc()
		defer m.InflightRequests.Dec()

		ctx, labels := LabelsForRequest(r.Context())
		ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Wrap the request body so we can count bytes received from the
		// client (client_rx_bytes_total + request_size_bytes). Skip
		// http.NoBody — the stdlib uses it as a sentinel for bodyless
		// requests (GET/HEAD); wrapping it would record a zero-byte
		// observation that skews the size histogram without adding signal.
		var reqBodyCounter *countingReadCloser
		if r.Body != nil && r.Body != http.NoBody {
			reqBodyCounter = &countingReadCloser{rc: r.Body}
			r.Body = reqBodyCounter
		}

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

		// Record client byte-transfer metrics. The request-body counter
		// is nil when the inbound request had no body (e.g. GET); in that
		// case there's nothing to observe for the rx / size metrics.
		if reqBodyCounter != nil {
			rxBytes := float64(reqBodyCounter.bytes)
			m.ClientRxBytesTotal.WithLabelValues(ns).Add(rxBytes)
			m.RequestSizeBytes.WithLabelValues(ns).Observe(rxBytes)
		}
		txBytes := float64(ww.bytesWritten)
		m.ClientTxBytesTotal.WithLabelValues(ns).Add(txBytes)
		m.ResponseSizeBytes.WithLabelValues(ns).Observe(txBytes)
	})
}

// statusRecorder captures the status code as it is written so the middleware
// can label metrics with it. WriteHeader is called at most once by stdlib
// semantics, and we mirror that.
type statusRecorder struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
	wroteHeader  bool
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
	n, err := s.ResponseWriter.Write(b)
	s.bytesWritten += int64(n)
	return n, err
}

// ReadFrom implements io.ReaderFrom. io.Copy prefers this method over
// Write when the source is not a WriterTo, so without it the underlying
// ResponseWriter's ReadFrom path (e.g. sendfile / splice fast paths)
// would silently bypass bytesWritten accounting. We delegate to the
// underlying writer's ReadFrom when it supports one; otherwise we copy
// through our own Write() via an io.Writer-only view of s, which
// prevents io.Copy from detecting that s implements ReaderFrom and
// recursing back into this method.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		// The delegate path commits the response (an implicit 200 when
		// no explicit WriteHeader preceded it). Record that so a later
		// WriteHeader on an error path cannot silently overwrite the
		// status the underlying ResponseWriter already sent.
		if n > 0 && !s.wroteHeader {
			s.status = http.StatusOK
			s.wroteHeader = true
		}
		s.bytesWritten += n
		return n, err
	}
	return io.Copy(struct{ io.Writer }{s}, r)
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

// countingReadCloser wraps an io.ReadCloser and counts bytes read through
// it. Used by the metrics middleware to track client request body sizes
// without interfering with the downstream handler's consumption of the
// body. Close is forwarded unchanged; byte accounting stops mattering
// once the body is closed because no further Read calls are expected.
type countingReadCloser struct {
	rc    io.ReadCloser
	bytes int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.bytes += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error {
	return c.rc.Close()
}
