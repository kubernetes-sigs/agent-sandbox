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
	"net/url"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	clientmetrics "k8s.io/client-go/tools/metrics"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// findHistogram returns the single histogram series for the named metric
// family in the controller-runtime registry, or nil if absent.
func findHistogram(t *testing.T, name string) *dto.Metric {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("expected 1 series for %s, got %d", name, len(mf.GetMetric()))
		}
		return mf.GetMetric()[0]
	}
	return nil
}

func TestRESTClientLatencyMetrics(t *testing.T) {
	requestLatency.Reset()
	rateLimiterLatency.Reset()

	u := url.URL{Scheme: "https", Host: "apiserver.example:6443", Path: "/api/v1/namespaces/default/pods"}
	// Observe through the client-go hooks to verify init() wired them up.
	clientmetrics.RequestLatency.Observe(context.Background(), "GET", u, 250*time.Millisecond)
	clientmetrics.RateLimiterLatency.Observe(context.Background(), "GET", u, 5*time.Millisecond)

	for _, name := range []string{
		"rest_client_request_duration_seconds",
		"rest_client_rate_limiter_duration_seconds",
	} {
		m := findHistogram(t, name)
		if m == nil {
			t.Fatalf("metric %s not registered in controller-runtime registry", name)
		}
		if got := m.GetHistogram().GetSampleCount(); got != 1 {
			t.Errorf("%s: expected 1 observation, got %d", name, got)
		}
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["verb"] != "GET" || labels["host"] != "apiserver.example:6443" {
			t.Errorf("%s: unexpected labels %v, want verb=GET host=apiserver.example:6443", name, labels)
		}
	}
}
