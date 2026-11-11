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
	"testing"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// failingStrategy is a ConnectionStrategy whose Connect always fails, so the
// failure path of connector.Connect can be exercised without a cluster.
type failingStrategy struct {
	err error
}

func (f *failingStrategy) Connect(context.Context) (string, error) { return "", f.err }
func (f *failingStrategy) Close() error                            { return nil }

// discoveryLatencyPoints collects the recorded histogram data points.
func discoveryLatencyPoints(t *testing.T, reader *metric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != discoveryLatencyInstrumentName {
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

	points := discoveryLatencyPoints(t, reader)
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
	// Buckets must match the Python SDK's _LATENCY_BUCKETS_MS so cross-SDK
	// dashboards aggregate identically.
	wantBounds := []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000}
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

	points := discoveryLatencyPoints(t, reader)
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
	// this instrument name as sandbox_client_discovery_latency_ms.
	if discoveryLatencyInstrumentName != "sandbox.client.discovery.latency.ms" {
		t.Errorf("instrument name = %q, want sandbox.client.discovery.latency.ms", discoveryLatencyInstrumentName)
	}

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	hist := newDiscoveryLatencyHistogram(mp.Meter("test"), logr.Discard())
	hist.Record(context.Background(), 123.0)

	if points := discoveryLatencyPoints(t, reader); len(points) != 1 {
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

	points := discoveryLatencyPoints(t, reader)
	if len(points) != 1 {
		t.Fatalf("got %d data points, want 1: Options.MeterProvider is not wired into the connector", len(points))
	}
	if !attrsContain(points[0].Attributes, "mode", "direct") || !attrsContain(points[0].Attributes, "status", "success") {
		t.Errorf("got attributes %v, want mode=direct status=success", points[0].Attributes)
	}
}
