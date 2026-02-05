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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	LaunchTypeWarm    = "warm"    // Pod from a SandboxWarmPool
	LaunchTypeCold    = "cold"    // Pod not from a SandboxWarmPool
	LaunchTypeUnknown = "unknown" // Used when Sandbox is nil during failure
	StatusSuccess     = "success"
	StatusFailure     = "failure"
)

var (
	// ClaimStartupLatency measures the time from SandboxClaim creation to Pod Ready state.
	// Labels:
	// - launch_type: "warm" or "cold"
	// - status: "success" or "failure"
	// - sandbox_template: the SandboxTemplateRef
	ClaimStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_startup_latency_ms",
			Help: "End-to-end latency from SandboxClaim creation to Pod Ready state in milliseconds.",
			// Buckets for latency from 50ms to 4 minutes
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "status", "sandbox_template"},
	)
)

// Init registers custom metrics with the global controller-runtime registry.
func init() {
	metrics.Registry.MustRegister(ClaimStartupLatency)
}

// RecordClaimStartupLatency records the duration since the provided start time.
func RecordClaimStartupLatency(startTime time.Time, launchType, status, templateName string) {
	duration := float64(time.Since(startTime).Milliseconds())
	ClaimStartupLatency.WithLabelValues(launchType, status, templateName).Observe(duration)
}
