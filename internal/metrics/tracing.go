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
	"encoding/json"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// TraceContextAnnotation is a JSON-serialized map of W3C Trace Context headers.
	// Example: {"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	TraceContextAnnotation = "opentelemetry.io/trace-context"
)

type noopInstrumenter struct{}

func (n *noopInstrumenter) StartSpan(ctx context.Context, _ metav1.Object, _ string, _ map[string]string) (context.Context, func()) {
	return ctx, func() {}
}
func (n *noopInstrumenter) GetTraceContext(_ context.Context) string                  { return "" }
func (n *noopInstrumenter) AddEvent(_ context.Context, _ string, _ map[string]string) {}
func (n *noopInstrumenter) IsRecording(_ context.Context) bool                        { return false }
func NewNoOp() Instrumenter                                                           { return &noopInstrumenter{} }

// Instrumenter defines the operations needed for tracing sandbox lifecycles.
type Instrumenter interface {
	StartSpan(ctx context.Context, obj metav1.Object, spanName string, attrs map[string]string) (context.Context, func())
	GetTraceContext(ctx context.Context) string
	AddEvent(ctx context.Context, name string, attrs map[string]string)
	IsRecording(ctx context.Context) bool
}

type otelInstrumenter struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	logger     logr.Logger
}

// StartSpan starts a span, potentially continuing one extracted from resource annotations.
func (o *otelInstrumenter) StartSpan(ctx context.Context, obj metav1.Object, spanName string, attrs map[string]string) (context.Context, func()) {

	// 1. Extract Parent Context from annotations if present.
	if obj != nil && obj.GetAnnotations() != nil {
		if tc, ok := obj.GetAnnotations()[TraceContextAnnotation]; ok && tc != "" {
			var carrier map[string]string
			if err := json.Unmarshal([]byte(tc), &carrier); err == nil {
				ctx = o.propagator.Extract(ctx, propagation.MapCarrier(carrier))
			} else {
				o.logger.Error(err, "failed to unmarshal trace context annotation", "annotation", tc)
			}
		}
	}

	// 2. Prepare initial attributes (WithAttributes)
	opts := []trace.SpanStartOption{}
	if len(attrs) > 0 {
		otelAttrs := make([]attribute.KeyValue, 0, len(attrs))
		for k, v := range attrs {
			otelAttrs = append(otelAttrs, attribute.String(k, v))
		}
		opts = append(opts, trace.WithAttributes(otelAttrs...))
	}

	// 3. Start Span with options
	ctx, span := o.tracer.Start(ctx, spanName, opts...)
	return ctx, func() { span.End() }
}

// GetTraceContext returns the current W3C context as a JSON string for persistence.
func (o *otelInstrumenter) GetTraceContext(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	o.propagator.Inject(ctx, carrier)
	data, err := json.Marshal(carrier)
	if err != nil {
		o.logger.Error(err, "failed to marshal trace context")
		return ""
	}
	return string(data)
}

// AddEvent uses WithAttributes to provide info about state changes or progress.
func (o *otelInstrumenter) AddEvent(ctx context.Context, name string, attrs map[string]string) {
	span := trace.SpanFromContext(ctx)
	otelAttrs := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		otelAttrs = append(otelAttrs, attribute.String(k, v))
	}

	span.AddEvent(name, trace.WithAttributes(otelAttrs...))
}

// Returns true if the span in the context is a real, sampled-in span.
func (o *otelInstrumenter) IsRecording(ctx context.Context) bool {
	return trace.SpanFromContext(ctx).IsRecording()
}

// SetupOTel initializes the global OpenTelemetry SDK and returns an Instrumenter.
func SetupOTel(ctx context.Context, serviceName string) (Instrumenter, func(), error) {
	// exporter respects OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_INSECURE env vars.
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(serviceName),
		)),
	)
	otel.SetTracerProvider(tp)
	// Use standard W3C Context propagator only (no Baggage).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return &otelInstrumenter{
		tracer:     tp.Tracer("agent-sandbox-controller"),
		propagator: otel.GetTextMapPropagator(),
		logger:     log.FromContext(ctx).WithName("tracing"),
	}, func() { _ = tp.Shutdown(context.Background()) }, nil
}
