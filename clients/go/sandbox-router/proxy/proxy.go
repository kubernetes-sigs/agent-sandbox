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

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/propagation"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/clients/go/sandbox-router/observability"
)

// Handler implements the request-routing core of the sandbox-router. Each
// HTTP request is parsed into a Target and proxied to the upstream sandbox
// with the same body, headers (minus Host), and method.
type Handler struct {
	cfg        *config.Config
	metrics    *observability.Metrics
	propagator propagation.TextMapPropagator
	transport  http.RoundTripper
	cache      Lookup
	log        logr.Logger
}

// Options bundles the dependencies NewHandler needs. Metrics, Propagator,
// and Cache are optional; nil values produce a router with no metrics, a
// no-op propagator, and DNS-only resolution respectively, which is
// convenient for tests.
type Options struct {
	Config     *config.Config
	Metrics    *observability.Metrics
	Propagator propagation.TextMapPropagator
	// Cache is the Pod-IP lookup used for the KEP-NNNN fast path. When
	// nil, the handler resolves every request via DNS — useful for tests
	// and for deployments running without RBAC for Pod informers.
	Cache  Lookup
	Logger logr.Logger
}

// NewHandler builds a Handler from o.
func NewHandler(o Options) *Handler {
	if o.Config == nil {
		panic("proxy.NewHandler: Config is required")
	}
	if o.Propagator == nil {
		o.Propagator = propagation.TraceContext{}
	}
	var tr http.RoundTripper = defaultTransport(o.Config)
	// Wrap with retry only if max-retries > 0. The transport is unchanged
	// when retries are disabled so the request path stays a single Dial.
	if o.Config.UpstreamMaxRetries > 0 {
		// Total attempts = 1 (initial) + UpstreamMaxRetries.
		attempts := 1 + o.Config.UpstreamMaxRetries
		var onRetry func(*http.Request, error, int)
		if o.Metrics != nil {
			metrics := o.Metrics
			onRetry = func(req *http.Request, _ error, _ int) {
				metrics.UpstreamRetriesTotal.WithLabelValues(
					observability.SandboxNamespaceFromContext(req.Context()),
				).Inc()
			}
		}
		tr = newRetryTransport(tr, attempts,
			o.Config.UpstreamRetryInitialDelay,
			o.Config.UpstreamRetryMaxDelay,
			onRetry,
		)
	}
	return &Handler{
		cfg:        o.Config,
		metrics:    o.Metrics,
		propagator: o.Propagator,
		transport:  tr,
		cache:      o.Cache,
		log:        o.Logger,
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, perr := ParseSandboxHeaders(r.Header)
	if perr != nil {
		WriteJSONError(w, perr)
		return
	}

	// Make the parsed namespace visible to the observability middleware.
	if labels := observability.LabelsFromContext(r.Context()); labels != nil {
		labels.SandboxNamespace = target.Namespace
	}

	target0 := target // capture for closures
	// Resolve once per request so the ErrorHandler can see which path
	// produced the IP (cache vs DNS vs override) and invalidate the cache
	// entry on dial-class failures. The Rewrite callback re-uses the URL.
	upstreamURL, src := target0.Resolve("http", h.cfg.ClusterDomain, r.URL.Path, r.URL.RawQuery, h.cache)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = upstreamURL
			// Clear inbound Host so net/http picks the URL host. Matches the
			// Python router's behavior of stripping Host before forwarding.
			pr.Out.Host = ""
			// Inject trace context into the outbound request so the sandbox
			// sees a continuation of the inbound trace.
			h.propagator.Inject(pr.Out.Context(), propagation.HeaderCarrier(pr.Out.Header))
		},
		Transport:     h.transport,
		FlushInterval: -1, // immediate flush for SSE / streaming responses
		ErrorHandler: func(w http.ResponseWriter, errReq *http.Request, err error) {
			reason := classifyError(err)
			h.recordUpstreamErrorReason(target0.Namespace, reason)
			// KEP-NNNN: actively invalidate the cache entry on dial-class
			// failures so the next request falls through to DNS instead of
			// retrying the same stale IP. We only invalidate when the IP
			// we tried actually came from the cache — a DNS or PodIP-header
			// failure means the cache had nothing useful to evict.
			if src == SourceCache && h.cache != nil && reason == "dial" && target0.UID != "" {
				if h.cache.Invalidate(types.UID(target0.UID)) && h.metrics != nil {
					h.metrics.CacheInvalidationsTotal.WithLabelValues(target0.Namespace).Inc()
				}
			}
			// Use the per-request logger from context so the trace ID is
			// included alongside the upstream failure detail.
			observability.LoggerFromContext(errReq.Context(), h.log).Error(err,
				"upstream connect failure",
				"sandbox", target0.ID,
				"namespace", target0.Namespace,
				"source", string(src),
			)
			WriteJSONError(w, &Error{
				Status: http.StatusBadGateway,
				Detail: fmt.Sprintf("Could not connect to the backend sandbox: %s", target0.ID),
			})
		},
	}

	// Bound the upstream request lifetime by the configured proxy timeout.
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ProxyTimeout)
	defer cancel()
	rp.ServeHTTP(w, r.WithContext(ctx))
}

// defaultTransport builds the shared *http.Transport used for upstream
// requests. Values mirror Go's DefaultTransport plus a configurable
// ResponseHeaderTimeout and disabled HTTP/2 to backends (sandboxes are
// h1 today; opting in to h2 to backends would require negotiation we don't
// want to introduce silently).
func defaultTransport(cfg *config.Config) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
	}
}

// recordUpstreamErrorReason bumps the upstream-error counter with a
// pre-classified reason label.
func (h *Handler) recordUpstreamErrorReason(namespace, reason string) {
	if h.metrics == nil {
		return
	}
	h.metrics.UpstreamErrorsTotal.WithLabelValues(namespace, reason).Inc()
}

// classifyError turns an arbitrary RoundTrip error into a low-cardinality
// label value so the upstream_errors_total counter does not explode.
func classifyError(err error) string {
	if err == nil {
		return "unknown"
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case strings.Contains(err.Error(), "tls"):
		return "tls"
	case strings.Contains(err.Error(), "EOF"):
		return "eof"
	case strings.Contains(err.Error(), "connection refused"),
		strings.Contains(err.Error(), "no such host"),
		strings.Contains(err.Error(), "dial"):
		return "dial"
	}
	return "other"
}
