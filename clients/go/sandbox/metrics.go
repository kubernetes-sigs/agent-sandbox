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
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Latency instrument names follow the OTel convention of keeping the unit
// out of the name and declaring it with metric.WithUnit instead. The OTel
// Prometheus exporter sanitizes dots to underscores and appends the unit, so
// these histograms export as sandbox_client_discovery_latency_seconds,
// sandbox_client_suspend_latency_seconds, and
// sandbox_client_resume_latency_seconds, the names the Python SDK's latency
// metrics will move to once the rename tracked in issue #2340 lands. The
// Python SDK's snapshot restore metric has no Go counterpart: the Go SDK
// does not support snapshots.
const (
	discoveryLatencyInstrumentName = "sandbox.client.discovery.latency"
	suspendLatencyInstrumentName   = "sandbox.client.suspend.latency"
	resumeLatencyInstrumentName    = "sandbox.client.resume.latency"
)

// latencyBucketsSeconds are the Python SDK's _LATENCY_BUCKETS_MS expressed
// in seconds, so both SDKs use the same bucket boundaries.
var latencyBucketsSeconds = []float64{
	0.1, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 5,
	10, 30, 60, 120, 240,
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

// newLatencyHistogram creates a latency histogram in seconds with the shared
// bucket boundaries. Instrument creation only fails for invalid names on a
// real provider; metrics must never break sandbox operations, so the error
// is logged and a noop histogram is returned instead.
func newLatencyHistogram(meter metric.Meter, log logr.Logger, name, description string) metric.Float64Histogram {
	hist, err := meter.Float64Histogram(
		name,
		metric.WithDescription(description),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(latencyBucketsSeconds...),
	)
	if err != nil {
		log.Error(err, "Failed to create latency histogram; sandbox client metrics are disabled", "instrument", name)
		return noop.Float64Histogram{}
	}
	return hist
}

// newDiscoveryLatencyHistogram creates the histogram that records how long
// connecting to a sandbox takes.
func newDiscoveryLatencyHistogram(meter metric.Meter, log logr.Logger) metric.Float64Histogram {
	return newLatencyHistogram(meter, log, discoveryLatencyInstrumentName,
		"Latency of establishing connection to the sandbox")
}

// newSuspendLatencyHistogram creates the histogram that records how long
// suspending a sandbox takes.
func newSuspendLatencyHistogram(meter metric.Meter, log logr.Logger) metric.Float64Histogram {
	return newLatencyHistogram(meter, log, suspendLatencyInstrumentName,
		"Latency of suspending the sandbox")
}

// newResumeLatencyHistogram creates the histogram that records how long
// resuming a sandbox takes.
func newResumeLatencyHistogram(meter metric.Meter, log logr.Logger) metric.Float64Histogram {
	return newLatencyHistogram(meter, log, resumeLatencyInstrumentName,
		"Latency of resuming the sandbox")
}

// canceledOperation reports whether a failed operation stopped because its
// context was canceled. Operations wrap the cause they observed when they
// stopped into their error, so the error alone classifies the outcome; a
// timeout wraps context.DeadlineExceeded and stays recorded as a failure.
func canceledOperation(err error) bool {
	return errors.Is(err, context.Canceled)
}

// recordLatency records the seconds elapsed since start on hist with a
// status attribute derived from err, mirroring the Python SDK's
// record_latency decorator. It records nothing when hist is nil or when the
// operation was canceled; see canceledOperation.
func recordLatency(ctx context.Context, hist metric.Float64Histogram, start time.Time, err error) {
	if hist == nil || canceledOperation(err) {
		return
	}
	status := "success"
	if err != nil {
		status = "failure"
	}
	hist.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("status", status)))
}
