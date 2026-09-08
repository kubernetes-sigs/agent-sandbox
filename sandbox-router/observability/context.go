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
	"context"

	"github.com/go-logr/logr"
)

// Labels carries per-request label values that the inner handler discovers
// after the outer observability middleware has already started. The
// middleware allocates a Labels value and attaches a pointer to it via
// WithLabels; the proxy handler mutates it once header parsing succeeds; the
// middleware reads it back when emitting metrics.
//
// Using a mutable struct via context pointer avoids the more common
// alternatives — re-parsing request headers in the middleware (couples
// packages and risks divergent default values), or type-asserting the
// ResponseWriter to a custom setter interface (fragile under wrapping).
type Labels struct {
	// SandboxNamespace is the resolved namespace of the request's routing
	// Target — from X-Sandbox-Namespace, or, when path-based routing is
	// enabled, the corresponding path segment (see ParsePathRoute). The
	// default sentinel "-" is used when no proxy handler has populated it
	// (e.g. for /healthz, /metrics, or requests rejected at parse time).
	SandboxNamespace string
	// SandboxID is the resolved sandbox ID of the same Target, from
	// whichever input actually produced it. Deliberately NOT read
	// straight from the X-Sandbox-Id header by consumers (tracing,
	// access logging): a path-routed request never sets that header at
	// all, and doing so would silently show an empty sandbox identity
	// for every browser-facing request. Empty when unset, same as
	// SandboxNamespace before the "-" sentinel is applied.
	SandboxID string
}

type labelsKey struct{}

// WithLabels attaches l to ctx. l must not be nil.
func WithLabels(ctx context.Context, l *Labels) context.Context {
	return context.WithValue(ctx, labelsKey{}, l)
}

// LabelsFromContext returns the *Labels attached by WithLabels, or nil.
func LabelsFromContext(ctx context.Context) *Labels {
	l, _ := ctx.Value(labelsKey{}).(*Labels)
	return l
}

// LabelsForRequest returns the *Labels already attached to ctx by an outer
// middleware, alongside ctx unchanged — or, if none is attached yet,
// allocates a fresh *Labels and returns the context it should be attached
// to for inner handlers.
//
// Every middleware that wants to observe the routing identity the proxy
// handler resolves (tracing, access logging, metrics) should call this
// once, rather than unconditionally allocating its own Labels: context
// values only propagate inward, so if two nested middleware layers each
// call WithLabels independently, only the innermost allocation is ever
// visible to the proxy handler that actually mutates it — the outer
// layer's own pointer is never touched and stays permanently empty. In the
// real middleware chain (TracingMiddleware, the outermost layer,
// allocating first) this makes every layer share one Labels instance;
// called standalone in a test with no outer layer present, it falls back
// to allocating its own, so each middleware still works correctly in
// isolation.
func LabelsForRequest(ctx context.Context) (context.Context, *Labels) {
	if l := LabelsFromContext(ctx); l != nil {
		return ctx, l
	}
	l := &Labels{}
	return WithLabels(ctx, l), l
}

// SandboxNamespaceFromContext returns the SandboxNamespace set on the Labels
// in ctx, or the sentinel "-" if no labels are attached or the field is
// unset.
func SandboxNamespaceFromContext(ctx context.Context) string {
	l := LabelsFromContext(ctx)
	if l == nil || l.SandboxNamespace == "" {
		return "-"
	}
	return l.SandboxNamespace
}

type loggerKey struct{}

// WithLogger attaches log to ctx so downstream handlers can pick it up with
// the per-request fields (trace_id, etc.) already baked in.
func WithLogger(ctx context.Context, log logr.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// LoggerFromContext returns the logger attached by WithLogger, or fallback
// if none was set. Callers should pass a sensible base logger as fallback so
// log lines aren't silently dropped when middleware ordering changes.
func LoggerFromContext(ctx context.Context, fallback logr.Logger) logr.Logger {
	if log, ok := ctx.Value(loggerKey{}).(logr.Logger); ok {
		return log
	}
	return fallback
}
