/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"net/url"
	"testing"
	"time"

	clientmetrics "k8s.io/client-go/tools/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// expectedOptInMetricFamilies lists the six opt-in REST client metric families
// that should be registered after calling RegisterRESTClientMetrics with all
// supported metric types.
var expectedOptInMetricFamilies = []string{
	"rest_client_request_duration_seconds",
	"rest_client_dns_resolution_duration_seconds",
	"rest_client_request_size_bytes",
	"rest_client_response_size_bytes",
	"rest_client_rate_limiter_duration_seconds",
	"rest_client_request_retries_total",
}

// allExpectedMetricFamilies includes both the opt-in metrics and the default
// rest_client_requests_total metric that is always registered by client-go.
var allExpectedMetricFamilies = append(append([]string{}, expectedOptInMetricFamilies...), "rest_client_requests_total")

// gatherMetricFamilyNames collects all metric family names from the
// controller-runtime metrics registry.
func gatherMetricFamilyNames(t *testing.T) map[string]struct{} {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics from registry: %v", err)
	}
	names := make(map[string]struct{}, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = struct{}{}
	}
	return names
}

// assertMetricFamiliesPresent verifies that all expected metric families are
// present in the gathered names map.
func assertMetricFamiliesPresent(t *testing.T, names map[string]struct{}, expected []string) {
	t.Helper()
	for _, name := range expected {
		if _, ok := names[name]; !ok {
			t.Errorf("Expected metric family %q to be registered, but not found in registry", name)
		}
	}
}

// observeAllRESTClientMetrics triggers representative observations for all
// REST client metrics via the client-go metrics adapters, ensuring each metric
// family has at least one data point in the registry.
func observeAllRESTClientMetrics(ctx context.Context) {
	clientmetrics.RequestResult.Increment(ctx, "200", "GET", "example.com")
	clientmetrics.RequestLatency.Observe(ctx, "GET", url.URL{Host: "example.com"}, 1*time.Second)
	clientmetrics.ResolverLatency.Observe(ctx, "example.com", 1*time.Second)
	clientmetrics.RequestSize.Observe(ctx, "GET", "example.com", 1024)
	clientmetrics.ResponseSize.Observe(ctx, "GET", "example.com", 1024)
	clientmetrics.RateLimiterLatency.Observe(ctx, "GET", url.URL{Host: "example.com"}, 1*time.Second)
	clientmetrics.RequestRetry.IncrementRetry(ctx, "200", "GET", "example.com")
}

// registerAllRESTClientMetrics registers all six opt-in REST client metrics,
// matching the call made in main().
func registerAllRESTClientMetrics() {
	metrics.RegisterRESTClientMetrics(
		metrics.MetricRequestLatency,
		metrics.MetricDNSResolutionLatency,
		metrics.MetricRequestSize,
		metrics.MetricResponseSize,
		metrics.MetricRateLimiterLatency,
		metrics.MetricRequestRetry,
	)
}

// TestRESTClientMetricsRegistration verifies that REST client metrics can be
// registered without errors and that all six expected opt-in metric families
// plus the default rest_client_requests_total are present in the
// controller-runtime metrics registry after triggering observations.
func TestRESTClientMetricsRegistration(t *testing.T) {
	registerAllRESTClientMetrics()

	ctx := context.Background()
	observeAllRESTClientMetrics(ctx)

	names := gatherMetricFamilyNames(t)
	assertMetricFamiliesPresent(t, names, allExpectedMetricFamilies)

	t.Logf("Successfully registered REST client metrics, found %d total metric families in registry", len(names))
}

// TestRESTClientMetricsIdempotent verifies that calling RegisterRESTClientMetrics
// multiple times does not cause errors (due to sync.Once protection) and that
// all metric families remain registered and gatherable after repeated calls.
func TestRESTClientMetricsIdempotent(t *testing.T) {
	// Call registration multiple times — should not panic or error.
	for i := range 3 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RegisterRESTClientMetrics panicked on call %d: %v", i, r)
				}
			}()
			registerAllRESTClientMetrics()
		}()
	}

	// Trigger observations to populate metrics.
	ctx := context.Background()
	observeAllRESTClientMetrics(ctx)

	// Verify all expected metric families remain present.
	names := gatherMetricFamilyNames(t)
	assertMetricFamiliesPresent(t, names, allExpectedMetricFamilies)

	t.Logf("Idempotence test passed, all %d expected metric families remain registered", len(allExpectedMetricFamilies))
}

// TestRESTClientMetricsControllerRuntimeRegistry verifies that REST client
// metrics are properly registered with controller-runtime's global metrics
// registry and can be gathered through the standard prometheus Gatherer
// interface. This ensures the metrics are exposed via the controller's
// monitoring endpoint alongside all other controller-runtime metrics.
func TestRESTClientMetricsControllerRuntimeRegistry(t *testing.T) {
	registerAllRESTClientMetrics()

	ctx := context.Background()
	observeAllRESTClientMetrics(ctx)

	// Use controller-runtime's registry directly as a prometheus.Gatherer.
	gatherer := metrics.Registry

	mfs, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics from controller-runtime registry: %v", err)
	}

	if len(mfs) == 0 {
		t.Fatal("Expected metrics to be gathered from controller-runtime registry, got 0")
	}

	// Build a set of found metric names and verify each expected family.
	found := make(map[string]int, len(mfs))
	for _, mf := range mfs {
		name := mf.GetName()
		found[name] = len(mf.GetMetric())
	}

	for _, name := range allExpectedMetricFamilies {
		count, ok := found[name]
		if !ok {
			t.Errorf("Expected metric family %q not found in controller-runtime registry", name)
			continue
		}
		t.Logf("Found metric family %q with %d series in controller-runtime registry", name, count)
	}
}
