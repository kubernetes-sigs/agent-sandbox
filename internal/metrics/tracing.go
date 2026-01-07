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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	TraceContextAnnotation = "opentelemetry.io/trace-context"
)

type noopInstrumenter struct{}

func (n *noopInstrumenter) StartSpan(ctx context.Context, _ metav1.Object, _ string) (context.Context, func()) {
	return ctx, func() {}
}
func (n *noopInstrumenter) GetTraceContext(_ context.Context) string { return "" }

func NewNoOp() Instrumenter { return &noopInstrumenter{} }

type Instrumenter interface {
	StartSpan(ctx context.Context, obj metav1.Object, spanName string) (context.Context, func())
	GetTraceContext(ctx context.Context) string
}

type otelInstrumenter struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

func (o *otelInstrumenter) StartSpan(ctx context.Context, obj metav1.Object, spanName string) (context.Context, func()) {
	if obj != nil && obj.GetAnnotations() != nil {
		if tc, ok := obj.GetAnnotations()[TraceContextAnnotation]; ok && tc != "" {
			var carrier map[string]string
			if err := json.Unmarshal([]byte(tc), &carrier); err == nil {
				ctx = o.propagator.Extract(ctx, propagation.MapCarrier(carrier))
			}
		}
	}
	ctx, span := o.tracer.Start(ctx, spanName)
	return ctx, func() { span.End() }
}

func (o *otelInstrumenter) GetTraceContext(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	o.propagator.Inject(ctx, carrier)
	data, _ := json.Marshal(carrier)
	return string(data)
}

func SetupOTel(ctx context.Context, serviceName string) (Instrumenter, func(), error) {
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
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return &otelInstrumenter{
		tracer:     tp.Tracer("agent-sandbox-controller"),
		propagator: otel.GetTextMapPropagator(),
	}, func() { _ = tp.Shutdown(context.Background()) }, nil
}
