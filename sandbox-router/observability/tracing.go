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
// trace context from inbound headers using prop, and decorates the span with
// the routing headers so per-sandbox traces are searchable.
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
			ctx, span := tracer.Start(ctx, "HTTP "+r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.target", r.URL.Path),
					attribute.String("sandbox.id", r.Header.Get("X-Sandbox-Id")),
					attribute.String("sandbox.namespace", r.Header.Get("X-Sandbox-Namespace")),
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
			span.SetAttributes(attribute.Int("http.status_code", ww.status))
		})
	}
}
