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
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTracingMiddleware_UsesResolvedIdentityNotHeaders is the regression
// test for the bug caught in review: sandbox.id/sandbox.namespace span
// attributes used to be read straight from the X-Sandbox-* request
// headers, which a path-routed request (browser traffic reaching the
// router via --path-routing-prefix, see ParsePathRoute) never sets at
// all — so every such request traced with an empty sandbox identity, no
// matter which sandbox it actually reached. The inner handler here sets
// Labels directly (exactly what the proxy Handler's ServeHTTP does once
// it resolves a Target, from either input) with NO X-Sandbox-* header on
// the request at all, simulating a path-routed request end to end.
func TestTracingMiddleware_UsesResolvedIdentityNotHeaders(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := tp.Tracer("test")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labels := LabelsFromContext(r.Context())
		if labels == nil {
			t.Fatal("inner handler: no *Labels attached by TracingMiddleware")
		}
		labels.SandboxID = "path-routed-box"
		labels.SandboxNamespace = "poc-agent-sandbox"
		w.WriteHeader(http.StatusOK)
	})

	mw := TracingMiddleware(tracer, propagation.TraceContext{}, logr.Discard())
	// No X-Sandbox-Id / X-Sandbox-Namespace header anywhere on this
	// request — that's the whole point.
	req := httptest.NewRequest(http.MethodGet, "/router/poc-agent-sandbox/path-routed-box/8080/", nil)
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(spans))
	}
	attrs := attribute.NewSet(spans[0].Attributes()...)

	wantID, ok := attrs.Value("sandbox.id")
	if !ok || wantID.AsString() != "path-routed-box" {
		t.Errorf("sandbox.id attribute: got %v (present=%v), want %q", wantID, ok, "path-routed-box")
	}
	wantNS, ok := attrs.Value("sandbox.namespace")
	if !ok || wantNS.AsString() != "poc-agent-sandbox" {
		t.Errorf("sandbox.namespace attribute: got %v (present=%v), want %q", wantNS, ok, "poc-agent-sandbox")
	}
	if status, ok := attrs.Value("http.status_code"); !ok || status.AsInt64() != http.StatusOK {
		t.Errorf("http.status_code attribute: got %v (present=%v), want %d", status, ok, http.StatusOK)
	}
}

// TestTracingMiddleware_EmptyIdentityWhenUnresolved makes sure a request
// that never reaches routing resolution (e.g. rejected before the inner
// handler runs) doesn't crash and simply reports an empty identity, rather
// than something stale or panicking on a nil Labels.
func TestTracingMiddleware_EmptyIdentityWhenUnresolved(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := tp.Tracer("test")

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	mw := TracingMiddleware(tracer, propagation.TraceContext{}, logr.Discard())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(spans))
	}
	attrs := attribute.NewSet(spans[0].Attributes()...)
	if v, ok := attrs.Value("sandbox.id"); !ok || v.AsString() != "" {
		t.Errorf("sandbox.id attribute: got %v (present=%v), want empty string", v, ok)
	}
}
