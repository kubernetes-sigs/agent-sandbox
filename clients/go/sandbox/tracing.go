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

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Span attribute keys in the sandbox.* namespace.
var (
	AttrClaimName        = attribute.Key("sandbox.claim.name")
	AttrCommand          = attribute.Key("sandbox.command")
	AttrExitCode         = attribute.Key("sandbox.exit_code")
	AttrFilePath         = attribute.Key("sandbox.file.path")
	AttrFileSize         = attribute.Key("sandbox.file.size")
	AttrFileCount        = attribute.Key("sandbox.file.count")
	AttrFileExists       = attribute.Key("sandbox.file.exists")
	AttrGatewayName      = attribute.Key("sandbox.gateway.name")
	AttrGatewayNamespace = attribute.Key("sandbox.gateway.namespace")
	AttrRequestID        = attribute.Key("sandbox.request_id")
)

// NewTracerProvider creates a TracerProvider with an OTLP/gRPC exporter.
// The endpoint is read from OTEL_EXPORTER_OTLP_ENDPOINT (default: localhost:4317).
// serviceName becomes the service.name resource attribute.
// The caller owns the returned provider and must call Shutdown when done.
func NewTracerProvider(ctx context.Context, serviceName string) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create OTLP trace exporter: %w", err)
	}

	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes("",
			attribute.String("service.name", serviceName),
		),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("sandbox: failed to create OTel resource: %w", err)
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	), nil
}

// startSpan creates a child span parented to whatever span is in ctx.
func startSpan(ctx context.Context, tracer trace.Tracer, svcName, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer.Start(ctx, svcName+"."+operation, trace.WithAttributes(attrs...))
}

// withLifecycleSpan injects the lifecycle span as parent into ctx.
func withLifecycleSpan(ctx context.Context, lifecycleCtx context.Context) context.Context {
	if lifecycleCtx != nil {
		return trace.ContextWithSpan(ctx, trace.SpanFromContext(lifecycleCtx))
	}
	return ctx
}

// recordError sets the span status to Error and records the error event.
func recordError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// traceContextJSON returns the W3C trace context as a JSON string for
// injection into Kubernetes annotations.
func traceContextJSON(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return ""
	}
	data, _ := json.Marshal(map[string]string(carrier))
	return string(data)
}

// newTracer configures a tracer from Options.
func newTracer(opts Options) (trace.Tracer, string) {
	svcName := opts.TraceServiceName

	var provider trace.TracerProvider
	if opts.TracerProvider != nil {
		provider = opts.TracerProvider
	} else {
		provider = otel.GetTracerProvider()
	}

	scope := strings.ReplaceAll(svcName, "-", "_")
	return provider.Tracer(scope), svcName
}
