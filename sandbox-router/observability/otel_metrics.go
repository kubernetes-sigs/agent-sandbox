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
	"fmt"

	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"github.com/prometheus/client_golang/prometheus"
)

// SetupOTLPMetrics installs an OTel MeterProvider that periodically pushes
// every series registered in reg via OTLP gRPC. The push runs alongside the
// Prometheus /metrics endpoint — the bridge is read-only, so existing pull
// scrapers are unaffected.
//
// Endpoint, headers, compression, and TLS are read from the standard
// OTEL_EXPORTER_OTLP_* environment variables (or the metrics-specific
// OTEL_EXPORTER_OTLP_METRICS_* set). Defaults follow the OTLP spec
// (localhost:4317, insecure unless TLS env vars say otherwise) — callers
// running in a cluster should set OTEL_EXPORTER_OTLP_ENDPOINT explicitly.
//
// The returned shutdown func flushes any in-flight metrics and tears the
// provider down; main() should defer it.
func SetupOTLPMetrics(ctx context.Context, serviceName string, reg *prometheus.Registry) (shutdown func(context.Context) error, err error) {
	if reg == nil {
		return nil, fmt.Errorf("prometheus registry must not be nil")
	}

	exporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}

	// The Prometheus bridge turns the Prometheus Gatherer into an OTel
	// MetricProducer. The periodic reader then drives the bridge on every
	// export tick, so we keep a single source of truth (the Prometheus
	// registry) for both pull and push consumers.
	producer := prombridge.NewMetricProducer(prombridge.WithGatherer(reg))
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithProducer(producer))

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("create OTel resource: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}
