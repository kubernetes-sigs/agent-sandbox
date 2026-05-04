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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClientGoHTTPAdapter_Increment(t *testing.T) {
	k8sClientHTTPRequestsTotal.Reset()

	adapter := &clientGoHTTPMetricAdapter{
		count:    k8sClientHTTPRequestsTotal,
		duration: k8sClientHTTPRequestDuration,
	}

	adapter.Increment(context.Background(), "200", "GET", "/api/v1/pods")
	adapter.Increment(context.Background(), "500", "GET", "/api/v1/pods")

	if got := testutil.CollectAndCount(k8sClientHTTPRequestsTotal); got != 2 {
		t.Errorf("Expected 2 status_code metrics, got %d", got)
	}
}

func TestClientGoHTTPAdapter_Observe(t *testing.T) {
	k8sClientHTTPRequestDuration.Reset()

	adapter := &clientGoHTTPMetricAdapter{
		count:    k8sClientHTTPRequestsTotal,
		duration: k8sClientHTTPRequestDuration,
	}

	u := url.URL{Path: "/api/v1/pods"}
	adapter.Observe(context.Background(), "GET", u, 500*time.Millisecond)

	if got := testutil.CollectAndCount(k8sClientHTTPRequestDuration); got != 1 {
		t.Errorf("Expected 1 duration metric, got %d", got)
	}
}

func TestClientGoRateLimiterAdapter_Observe(t *testing.T) {
	k8sClientRateLimiterDuration.Reset()

	adapter := &clientGoRateLimiterMetricAdapter{
		duration: k8sClientRateLimiterDuration,
	}

	u := url.URL{Path: "/api/v1/pods"}
	adapter.Observe(context.Background(), "GET", u, 1*time.Second)

	if got := testutil.CollectAndCount(k8sClientRateLimiterDuration); got != 1 {
		t.Errorf("Expected 1 rate limiter metric, got %d", got)
	}
}

func TestMustRegisterClientGoMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	MustRegisterClientGoMetrics(registry)

	// Reset and record data so GatherAndCount can verify the metrics are registered.
	k8sClientHTTPRequestsTotal.Reset()
	k8sClientHTTPRequestDuration.Reset()
	k8sClientRateLimiterDuration.Reset()
	k8sClientHTTPRequestsTotal.WithLabelValues("200").Inc()
	k8sClientHTTPRequestDuration.WithLabelValues("/api/v1/pods").Observe(0.5)
	k8sClientRateLimiterDuration.WithLabelValues("/api/v1/pods").Observe(0.5)

	// Verify all three metrics are registered in the provided registry.
	if got, err := testutil.GatherAndCount(registry, "agent_sandbox_k8s_client_http_requests_total"); err != nil {
		t.Fatal(err)
	} else if got != 1 {
		t.Errorf("Expected 1 registered metric for k8s_client_http_requests_total, got %d", got)
	}
	if got, err := testutil.GatherAndCount(registry, "agent_sandbox_k8s_client_http_request_duration_seconds"); err != nil {
		t.Fatal(err)
	} else if got != 1 {
		t.Errorf("Expected 1 registered metric for k8s_client_http_request_duration_seconds, got %d", got)
	}
	if got, err := testutil.GatherAndCount(registry, "agent_sandbox_k8s_client_rate_limiter_duration_seconds"); err != nil {
		t.Fatal(err)
	} else if got != 1 {
		t.Errorf("Expected 1 registered metric for k8s_client_rate_limiter_duration_seconds, got %d", got)
	}
}
