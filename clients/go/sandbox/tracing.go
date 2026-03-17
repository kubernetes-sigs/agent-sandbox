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
	"sync"

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
)

var (
	globalProvider   *sdktrace.TracerProvider
	globalShutdown   func(context.Context) error
	globalProviderMu sync.Mutex
)

// InitTracer initializes a global OpenTelemetry TracerProvider with an
// OTLP/gRPC exporter. Only the first call takes effect; subsequent calls
// return the existing provider's shutdown function. The endpoint is
// controlled by OTEL_EXPORTER_OTLP_ENDPOINT (default: localhost:4317).
//
//	shutdown, err := sandbox.InitTracer(ctx, "my-service")
//	if err != nil { ... }
//	defer shutdown(ctx)
func InitTracer(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error) {
	globalProviderMu.Lock()
	defer globalProviderMu.Unlock()

	if globalProvider != nil {
		return globalProvider.Shutdown, nil
	}

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

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	globalProvider = provider
	globalShutdown = provider.Shutdown
	return provider.Shutdown, nil
}

// ShutdownTracer shuts down the global TracerProvider initialized by
// InitTracer, flushing any remaining spans and closing the exporter
// connection. This is a no-op if InitTracer was never called or if
// a custom TracerProvider was used. Call this at program exit.
func ShutdownTracer(ctx context.Context) error {
	globalProviderMu.Lock()
	shutdown := globalShutdown
	globalProvider = nil
	globalShutdown = nil
	globalProviderMu.Unlock()
	if shutdown != nil {
		return shutdown(ctx)
	}
	return nil
}

// startSpan creates a child span of the lifecycle span (if active) or the
// passed context.
func (c *SandboxClient) startSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	c.mu.Lock()
	if c.lifecycleCtx != nil {
		ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(c.lifecycleCtx))
	}
	c.mu.Unlock()
	return c.tracer.Start(ctx, c.traceServiceName+"."+operation, trace.WithAttributes(attrs...))
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

// initTracer configures the client's tracer from Options.
func (c *SandboxClient) initTracer() {
	c.traceServiceName = c.opts.TraceServiceName

	var provider trace.TracerProvider
	if c.opts.TracerProvider != nil {
		provider = c.opts.TracerProvider
	} else {
		provider = otel.GetTracerProvider()
	}

	scope := strings.ReplaceAll(c.traceServiceName, "-", "_")
	c.tracer = provider.Tracer(scope)
}
