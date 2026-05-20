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
	log        logr.Logger
}

// Options bundles the dependencies NewHandler needs. metrics and propagator
// are optional; nil values produce a router with no metrics and a no-op
// propagator, which is convenient for tests.
type Options struct {
	Config     *config.Config
	Metrics    *observability.Metrics
	Propagator propagation.TextMapPropagator
	Logger     logr.Logger
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
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target0.UpstreamURL("http", h.cfg.ClusterDomain, r.URL.Path, r.URL.RawQuery)
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
			h.recordUpstreamError(target0.Namespace, err)
			// Use the per-request logger from context so the trace ID is
			// included alongside the upstream failure detail.
			observability.LoggerFromContext(errReq.Context(), h.log).Error(err,
				"upstream connect failure",
				"sandbox", target0.ID,
				"namespace", target0.Namespace,
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

// recordUpstreamError categorizes err and bumps the upstream-error counter.
func (h *Handler) recordUpstreamError(namespace string, err error) {
	if h.metrics == nil {
		return
	}
	h.metrics.UpstreamErrorsTotal.WithLabelValues(namespace, classifyError(err)).Inc()
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
