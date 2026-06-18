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

package observability

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// accessLogRecorder captures the response status and byte count for the
// access log middleware. It's distinct from the metrics middleware's
// statusRecorder so the two middlewares stay independently composable.
type accessLogRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *accessLogRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *accessLogRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying ResponseWriter so streaming responses
// (httputil.ReverseProxy with FlushInterval=-1) keep working.
func (r *accessLogRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter when it supports
// hijacking. Required for protocol upgrades — httputil.ReverseProxy
// type-asserts http.Hijacker on the ResponseWriter it gets and bails
// out of the upgrade path if the assertion fails. Without this method
// our wrapping middleware silently breaks every WebSocket the router
// is supposed to carry. Returns http.ErrNotSupported when wrapping a
// ResponseWriter that itself doesn't support hijacking (e.g. HTTP/2
// streams).
func (r *accessLogRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Push forwards to the underlying ResponseWriter when it supports
// HTTP/2 server push. We don't use push ourselves; this just keeps
// the capability from being stripped by our middleware.
func (r *accessLogRecorder) Push(target string, opts *http.PushOptions) error {
	if p, ok := r.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap exposes the underlying ResponseWriter for the stdlib's
// http.ResponseController helper and any future middleware that wants
// to peel back the wrapping. Go 1.20+ uses this to find Flush/Hijack
// implementations when middleware doesn't expose them directly — we
// expose them anyway, but Unwrap is cheap insurance.
func (r *accessLogRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// AccessLogMiddleware emits one structured log line per inbound request.
// The logger is taken from the request context (if a per-request logger was
// attached upstream, e.g. by TracingMiddleware with the trace_id baked in)
// and falls back to base.
//
// skip returns true for requests that should not be logged. Pass nil to log
// every request; the typical caller passes a function that ignores
// frequently-polled health endpoints so they don't drown the logs.
func AccessLogMiddleware(base logr.Logger, skip func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &accessLogRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			log := LoggerFromContext(r.Context(), base)
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"client_ip", clientIP(r),
				"sandbox_id", r.Header.Get("X-Sandbox-Id"),
				"sandbox_namespace", r.Header.Get("X-Sandbox-Namespace"),
				"bytes_out", rec.bytes,
				"user_agent", r.UserAgent(),
			)
		})
	}
}

// SkipHealthAndMetrics is a convenience skip function for the access log
// middleware. The router's /healthz on the proxy port is hit by the Gateway
// HealthCheckPolicy on every cycle; logging that flood would drown signal.
func SkipHealthAndMetrics(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

// clientIP returns the source IP from r.RemoteAddr. v1 does NOT honor
// X-Forwarded-For — supporting that safely requires configuring a trusted
// proxy chain, which we defer until there's a concrete deployment story.
// Operators behind an L7 LB that rewrites Host should consider that LB's
// own access logs as authoritative for client IP today.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
