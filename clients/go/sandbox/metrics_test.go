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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakedynamic "k8s.io/client-go/dynamic/fake"
)

// failingStrategy is a ConnectionStrategy whose Connect always fails, so the
// failure path of connector.Connect can be exercised without a cluster.
type failingStrategy struct {
	err error
}

func (f *failingStrategy) Connect(context.Context) (string, error) { return "", f.err }
func (f *failingStrategy) Close() error                            { return nil }

// latencyPoints collects the recorded data points of one histogram.
func latencyPoints(t *testing.T, reader *metric.ManualReader, instrumentName string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != instrumentName {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %s has data type %T, want Histogram[float64]", m.Name, m.Data)
			}
			points = append(points, h.DataPoints...)
		}
	}
	return points
}

func attrsContain(set attribute.Set, key, value string) bool {
	v, ok := set.Value(attribute.Key(key))
	return ok && v.AsString() == value
}

func TestConnectorConnect_RecordsDiscoveryLatencyOnSuccess(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())

	conn := newConnector(connectorConfig{
		Strategy:         &DirectStrategy{URL: "http://127.0.0.1:8080"},
		Namespace:        "default",
		Log:              logr.Discard(),
		DiscoveryLatency: hist,
	})
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	points := latencyPoints(t, reader, discoveryLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1", len(points))
	}
	p := points[0]
	if p.Count != 1 {
		t.Errorf("got count %d, want 1", p.Count)
	}
	if !attrsContain(p.Attributes, "mode", "direct") {
		t.Errorf("got attributes %v, want mode=direct", p.Attributes)
	}
	if !attrsContain(p.Attributes, "status", "success") {
		t.Errorf("got attributes %v, want status=success", p.Attributes)
	}
	// Buckets must be the Python SDK's _LATENCY_BUCKETS_MS expressed in
	// seconds so cross-SDK dashboards aggregate identically.
	wantBounds := []float64{0.1, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 5, 10, 30, 60, 120, 240}
	if len(p.Bounds) != len(wantBounds) {
		t.Fatalf("got %d bucket bounds, want %d: %v", len(p.Bounds), len(wantBounds), p.Bounds)
	}
	for i, b := range wantBounds {
		if p.Bounds[i] != b {
			t.Errorf("bound[%d] = %v, want %v", i, p.Bounds[i], b)
		}
	}
}

func TestConnectorConnect_RecordsDiscoveryLatencyOnFailure(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())

	wantErr := errors.New("discovery failed")
	conn := newConnector(connectorConfig{
		Strategy:         &failingStrategy{err: wantErr},
		Namespace:        "default",
		Log:              logr.Discard(),
		DiscoveryLatency: hist,
	})
	err := conn.Connect(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Connect error = %v, want %v", err, wantErr)
	}

	points := latencyPoints(t, reader, discoveryLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1", len(points))
	}
	if points[0].Count != 1 {
		t.Errorf("got count %d, want 1", points[0].Count)
	}
	if !attrsContain(points[0].Attributes, "status", "failure") {
		t.Errorf("got attributes %v, want status=failure", points[0].Attributes)
	}
}

func TestConnectorConnect_NilHistogramDoesNotPanic(t *testing.T) {
	conn := newConnector(connectorConfig{
		Strategy: &DirectStrategy{URL: "http://127.0.0.1:8080"},
		Log:      logr.Discard(),
	})
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestStrategyMode(t *testing.T) {
	tests := []struct {
		name     string
		strategy ConnectionStrategy
		want     string
	}{
		{"gateway", &gatewayStrategy{}, "gateway"},
		{"port-forward", &tunnelStrategy{}, "port-forward"},
		{"router port-forward", &tunnelStrategy{routerREST: true}, string(ConnectivityRouterPortForward)},
		{"pod tunnel", &podTunnelStrategy{}, "sandboxd-pod-tunnel"},
		{"in-cluster pod IP", &inClusterStrategy{}, "in-cluster-pod-ip"},
		{"in-cluster service", &inClusterStrategy{useServiceDNS: true}, "in-cluster-service"},
		{"direct", &DirectStrategy{}, "direct"},
		// ConnectionStrategy is exported, so callers can bring their own
		// implementation. Labeling those "direct" would misreport the mode.
		{"custom strategy", &failingStrategy{}, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strategyMode(tt.strategy); got != tt.want {
				t.Errorf("strategyMode(%T) = %q, want %q", tt.strategy, got, tt.want)
			}
		})
	}
}

func TestNewMeter_NilProviderFallsBackToGlobal(t *testing.T) {
	// A nil MeterProvider must fall back to otel.GetMeterProvider() (noop by
	// default) instead of panicking, mirroring newTracer.
	m := newMeter(Options{TraceServiceName: "sandbox-client"})
	if m == nil {
		t.Fatal("newMeter returned nil")
	}
	hist := newDiscoveryLatencyHistogram(m, logr.Discard())
	// Recording on the noop meter must be safe.
	hist.Record(context.Background(), 1.0)
}

func TestNewDiscoveryLatencyHistogram_InstrumentName(t *testing.T) {
	// Guards the cross-SDK contract: the OTel Prometheus exporter renders
	// this instrument name and its "s" unit as
	// sandbox_client_discovery_latency_seconds.
	if discoveryLatencyInstrumentName != "sandbox.client.discovery.latency" {
		t.Errorf("instrument name = %q, want sandbox.client.discovery.latency", discoveryLatencyInstrumentName)
	}

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())
	hist.Record(context.Background(), 123.0)

	if points := latencyPoints(t, reader, discoveryLatencyInstrumentName); len(points) != 1 {
		t.Errorf("got %d data points, want 1", len(points))
	}
}

func TestNew_WiresMeterProviderToConnector(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	sb, err := New(context.Background(), Options{
		K8sHelper:     &K8sHelper{},
		APIURL:        "http://127.0.0.1:8080",
		MeterProvider: mp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sb.connector.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	points := latencyPoints(t, reader, discoveryLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1: Options.MeterProvider is not wired into the connector", len(points))
	}
	if !attrsContain(points[0].Attributes, "mode", "direct") || !attrsContain(points[0].Attributes, "status", "success") {
		t.Errorf("got attributes %v, want mode=direct status=success", points[0].Attributes)
	}
}

func TestSuspendResumeLatencyHistograms_InstrumentNames(t *testing.T) {
	// Guards the cross-SDK contract: the OTel Prometheus exporter renders
	// these instrument names and their "s" unit as
	// sandbox_client_suspend_latency_seconds and
	// sandbox_client_resume_latency_seconds.
	if suspendLatencyInstrumentName != "sandbox.client.suspend.latency" {
		t.Errorf("instrument name = %q, want sandbox.client.suspend.latency", suspendLatencyInstrumentName)
	}
	if resumeLatencyInstrumentName != "sandbox.client.resume.latency" {
		t.Errorf("instrument name = %q, want sandbox.client.resume.latency", resumeLatencyInstrumentName)
	}
}

func TestRecordLatency_StatusAttribute(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newSuspendLatencyHistogram(mp.Meter("test"), logr.Discard())

	recordLatency(context.Background(), hist, time.Now(), nil)
	recordLatency(context.Background(), hist, time.Now(), errors.New("boom"))

	points := latencyPoints(t, reader, suspendLatencyInstrumentName)
	if len(points) != 2 {
		t.Fatalf("got %d data points, want 2", len(points))
	}
	var sawSuccess, sawFailure bool
	for _, p := range points {
		if p.Count != 1 {
			t.Errorf("got count %d, want 1", p.Count)
		}
		sawSuccess = sawSuccess || attrsContain(p.Attributes, "status", "success")
		sawFailure = sawFailure || attrsContain(p.Attributes, "status", "failure")
	}
	if !sawSuccess || !sawFailure {
		t.Errorf("got points %v, want one status=success and one status=failure", points)
	}
}

func TestRecordLatency_NilHistogramDoesNotPanic(_ *testing.T) {
	recordLatency(context.Background(), nil, time.Now(), nil)
}

func TestRecordLatency_SkipsCanceledOperations(t *testing.T) {
	// A canceled operation stops at an arbitrary point, so its duration
	// must not land in the histogram as a failure sample. The error reaches
	// recordLatency wrapped, so the check uses errors.Is.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newSuspendLatencyHistogram(mp.Meter("test"), logr.Discard())

	recordLatency(context.Background(), hist, time.Now(),
		fmt.Errorf("operation cancelled waiting for lifecycle lock: %w", context.Canceled))

	if points := latencyPoints(t, reader, suspendLatencyInstrumentName); len(points) != 0 {
		t.Errorf("got %d data points, want 0 for a canceled operation", len(points))
	}
}

func TestRecordLatency_RecordsDeadlineExceeded(t *testing.T) {
	// A timeout is a real failure: the operation ran to its full budget, so
	// the duration is meaningful and must stay in the histogram.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newSuspendLatencyHistogram(mp.Meter("test"), logr.Discard())

	recordLatency(context.Background(), hist, time.Now(),
		fmt.Errorf("timed out: %w", context.DeadlineExceeded))

	points := latencyPoints(t, reader, suspendLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1", len(points))
	}
	if !attrsContain(points[0].Attributes, "status", "failure") {
		t.Errorf("got attributes %v, want status=failure", points[0].Attributes)
	}
}

func TestConnectorConnect_SkipsCanceledDiscovery(t *testing.T) {
	// connector.Connect records discovery latency inline, so it needs the
	// same cancellation skip as recordLatency.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())

	conn := newConnector(connectorConfig{
		Strategy:         &failingStrategy{err: context.Canceled},
		Namespace:        "default",
		Log:              logr.Discard(),
		DiscoveryLatency: hist,
	})
	if err := conn.Connect(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context.Canceled", err)
	}

	if points := latencyPoints(t, reader, discoveryLatencyInstrumentName); len(points) != 0 {
		t.Errorf("got %d data points, want 0 for a canceled discovery", len(points))
	}
}

// newGatewayTestConnector builds a connector with a real gatewayStrategy
// backed by a fake dynamic client that serves no Gateway, so discovery can
// only end in a timeout.
func newGatewayTestConnector(t *testing.T, hist apimetric.Float64Histogram, timeout time.Duration) *connector {
	t.Helper()
	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	return newConnector(connectorConfig{
		Strategy: &gatewayStrategy{
			dynamicClient:    dynCS,
			gatewayName:      "test-gw",
			gatewayNamespace: "default",
			gatewayScheme:    "http",
			timeout:          timeout,
			log:              logr.Discard(),
			tracer:           otel.GetTracerProvider().Tracer("test"),
		},
		Namespace:        "default",
		Log:              logr.Discard(),
		DiscoveryLatency: hist,
	})
}

func TestConnectorConnect_SkipsCanceledGatewayDiscovery(t *testing.T) {
	// gatewayStrategy.Connect reports any ctx.Done() as ErrTimeout without
	// wrapping context.Canceled, so the cancellation check must also look
	// at the context.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())
	conn := newGatewayTestConnector(t, hist, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := conn.Connect(ctx); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Connect error = %v, want ErrTimeout", err)
	}

	if points := latencyPoints(t, reader, discoveryLatencyInstrumentName); len(points) != 0 {
		t.Errorf("got %d data points, want 0 for a canceled gateway discovery", len(points))
	}
}

func TestConnectorConnect_RecordsGatewayTimeout(t *testing.T) {
	// A gateway timeout runs to the full budget, so it must stay recorded
	// as a failure.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())
	conn := newGatewayTestConnector(t, hist, 50*time.Millisecond)

	if err := conn.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Connect error = %v, want ErrTimeout", err)
	}

	points := latencyPoints(t, reader, discoveryLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1", len(points))
	}
	if !attrsContain(points[0].Attributes, "status", "failure") {
		t.Errorf("got attributes %v, want status=failure", points[0].Attributes)
	}
	if !attrsContain(points[0].Attributes, "mode", "gateway") {
		t.Errorf("got attributes %v, want mode=gateway", points[0].Attributes)
	}
}

func TestSuspend_RecordsFailureLatency(t *testing.T) {
	// A handle that never went through Open has no sandbox name, so Suspend
	// fails before touching the API server; the failure must still be
	// recorded, matching the Python SDK's decorator on the whole method.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	sb, err := New(context.Background(), Options{
		K8sHelper:     &K8sHelper{},
		APIURL:        "http://127.0.0.1:8080",
		MeterProvider: mp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sb.Suspend(context.Background()); err == nil {
		t.Fatal("Suspend on an unresolved sandbox succeeded, want error")
	}

	points := latencyPoints(t, reader, suspendLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1", len(points))
	}
	if !attrsContain(points[0].Attributes, "status", "failure") {
		t.Errorf("got attributes %v, want status=failure", points[0].Attributes)
	}
}

func TestResume_RecordsFailureLatency(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	sb, err := New(context.Background(), Options{
		K8sHelper:     &K8sHelper{},
		APIURL:        "http://127.0.0.1:8080",
		MeterProvider: mp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sb.Resume(context.Background()); err == nil {
		t.Fatal("Resume on an unresolved sandbox succeeded, want error")
	}

	points := latencyPoints(t, reader, resumeLatencyInstrumentName)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1", len(points))
	}
	if !attrsContain(points[0].Attributes, "status", "failure") {
		t.Errorf("got attributes %v, want status=failure", points[0].Attributes)
	}
}
