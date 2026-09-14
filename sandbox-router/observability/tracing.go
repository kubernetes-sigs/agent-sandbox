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
	"net/http"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TracingMiddleware opens a server span for every inbound request, extracts
// trace context from inbound headers using prop, and decorates the span
// with the resolved sandbox identity so per-sandbox traces are searchable.
//
// That identity is read from Labels — via LabelsForRequest, allocated here
// since this is the outermost layer in the real middleware chain — and
// applied to the span only AFTER next.ServeHTTP returns, alongside the
// existing http.status_code attribute: the proxy handler is what resolves
// the Target (from X-Sandbox-* headers, or, when path-based routing is
// enabled, from the URL path instead — see ParsePathRoute), and that
// resolution hasn't happened yet at span-start time. Reading
// X-Sandbox-Id/-Namespace straight from the request headers here, as an
// earlier version of this middleware did, silently produced empty
// sandbox.id/sandbox.namespace attributes for every path-routed request.
//
// When base is non-zero, a per-request logger is derived from it with the
// trace_id and span_id baked in as fields, and stashed in the request
// context for downstream handlers (notably the access log middleware and
// proxy ErrorHandler) to pick up via LoggerFromContext.
//
// The tracer and propagator are passed in (rather than reading globals at
// each request) so tests can wire deterministic no-op providers without
// touching the OTel global state.
func TracingMiddleware(tracer trace.Tracer, prop propagation.TextMapPropagator, base logr.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, labels := LabelsForRequest(ctx)
			ctx, span := tracer.Start(ctx, "HTTP "+r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.target", r.URL.Path),
				))
			defer span.End()

			// Attach a per-request logger with the trace ids as fields, so
			// every downstream log line emitted by access logging or the
			// proxy error handler is correlatable to its span in OTel.
			sc := span.SpanContext()
			if sc.IsValid() {
				reqLog := base.WithValues(
					"trace_id", sc.TraceID().String(),
					"span_id", sc.SpanID().String(),
				)
				ctx = WithLogger(ctx, reqLog)
			} else {
				ctx = WithLogger(ctx, base)
			}

			ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r.WithContext(ctx))
			span.SetAttributes(
				attribute.Int("http.status_code", ww.status),
				attribute.String("sandbox.id", labels.SandboxID),
				attribute.String("sandbox.namespace", labels.SandboxNamespace),
			)
		})
	}
}
