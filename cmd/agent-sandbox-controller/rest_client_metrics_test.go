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
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TestRESTClientMetricsRegistration verifies that REST client metrics can be
// registered without errors and that the metrics registry remains functional.
func TestRESTClientMetricsRegistration(t *testing.T) {
	// Register the REST client metrics (same as in main())
	metrics.RegisterRESTClientMetrics(
		metrics.MetricRequestLatency,
		metrics.MetricDNSResolutionLatency,
		metrics.MetricRequestSize,
		metrics.MetricResponseSize,
		metrics.MetricRateLimiterLatency,
		metrics.MetricRequestRetry,
	)

	// Verify the registry is still functional
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics after registration: %v", err)
	}

	// Verify we have some metrics registered
	if len(mfs) == 0 {
		t.Errorf("Expected some metrics to be registered, got 0")
	}

	t.Logf("Successfully registered REST client metrics, found %d total metrics in registry", len(mfs))
}

// TestRESTClientMetricsIdempotent verifies that calling RegisterRESTClientMetrics
// multiple times does not cause errors (due to sync.Once protection).
func TestRESTClientMetricsIdempotent(t *testing.T) {
	// Call registration multiple times - should not panic or error
	for i := range 3 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RegisterRESTClientMetrics panicked on call %d: %v", i, r)
				}
			}()
			metrics.RegisterRESTClientMetrics(
				metrics.MetricRequestLatency,
				metrics.MetricDNSResolutionLatency,
				metrics.MetricRequestSize,
				metrics.MetricResponseSize,
				metrics.MetricRateLimiterLatency,
				metrics.MetricRequestRetry,
			)
		}()
	}

	// Verify we can still gather metrics without errors
	_, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics after multiple registrations: %v", err)
	}
}
