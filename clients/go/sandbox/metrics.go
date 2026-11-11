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
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// discoveryLatencyInstrumentName mirrors the Python SDK's
// sandbox_client_discovery_latency_ms. The OTel Prometheus exporter
// sanitizes dots to underscores, so exporting this histogram yields the
// same metric name in both SDKs. The unit is deliberately left unset and
// values are recorded in milliseconds, because the exporter would
// otherwise append a unit suffix and break the shared name.
const discoveryLatencyInstrumentName = "sandbox.client.discovery.latency.ms"

// discoveryLatencyBucketsMs matches the Python SDK's _LATENCY_BUCKETS_MS so
// cross-SDK dashboards aggregate identically.
var discoveryLatencyBucketsMs = []float64{
	100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000,
	10000, 30000, 60000, 120000, 240000,
}

// newMeter configures a meter from Options, mirroring newTracer.
func newMeter(opts Options) metric.Meter {
	var provider metric.MeterProvider
	if opts.MeterProvider != nil {
		provider = opts.MeterProvider
	} else {
		provider = otel.GetMeterProvider()
	}
	scope := strings.ReplaceAll(opts.TraceServiceName, "-", "_")
	return provider.Meter(scope)
}

// newDiscoveryLatencyHistogram creates the histogram that records how long
// connecting to a sandbox takes. Instrument creation only fails for invalid
// names on a real provider; metrics must never break connectivity, so the
// error is logged and a noop histogram is returned instead.
func newDiscoveryLatencyHistogram(meter metric.Meter, log logr.Logger) metric.Float64Histogram {
	hist, err := meter.Float64Histogram(
		discoveryLatencyInstrumentName,
		metric.WithDescription("Latency of establishing connection to the sandbox in milliseconds"),
		metric.WithExplicitBucketBoundaries(discoveryLatencyBucketsMs...),
	)
	if err != nil {
		log.Error(err, "Failed to create discovery latency histogram; sandbox client metrics are disabled")
		return noop.Float64Histogram{}
	}
	return hist
}
