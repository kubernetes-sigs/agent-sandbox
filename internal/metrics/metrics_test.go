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

// nolint:revive
package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClaimLatencyRecording(t *testing.T) {
	testCases := []struct {
		name       string
		launchType string
	}{
		{"Warm", LaunchTypeWarm},
		{"Cold", LaunchTypeCold},
		{"Unknown", LaunchTypeUnknown},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ClaimStartupLatency.Reset()
			ClaimStartupLatency.WithLabelValues(tc.launchType, "test-tmpl").Observe(1000)

			if testutil.CollectAndCount(ClaimStartupLatency) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestSandboxClaimCreationRecording(t *testing.T) {
	testCases := []struct {
		name         string
		launchType   string
		podCondition string
	}{
		{"WarmReady", LaunchTypeWarm, "ready"},
		{"WarmNotReady", LaunchTypeWarm, "not_ready"},
		{"Cold", LaunchTypeCold, "not_ready"},
		{"Unknown", LaunchTypeUnknown, "not_ready"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			SandboxClaimCreationTotal.Reset()
			SandboxClaimCreationTotal.WithLabelValues("test-tmpl", tc.launchType, "test-pool", tc.podCondition).Inc()

			if testutil.CollectAndCount(SandboxClaimCreationTotal) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}
