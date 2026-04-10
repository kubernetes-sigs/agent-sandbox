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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/agent-sandbox/internal/version"
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

func TestClientClaimLatencyRecording(t *testing.T) {
	ctx := context.Background()
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
			ClientClaimStartupLatency.Reset()
			RecordClientClaimStartupLatency(ctx, time.Now().Add(-1*time.Second), tc.launchType, "test-tmpl")

			if testutil.CollectAndCount(ClientClaimStartupLatency) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestSandboxCreationLatencyRecording(t *testing.T) {
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
			SandboxCreationLatency.Reset()
			RecordSandboxCreationLatency(1000*time.Millisecond, "default", tc.launchType, "test-tmpl")

			if testutil.CollectAndCount(SandboxCreationLatency) != 1 {
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
			SandboxClaimCreationTotal.WithLabelValues("default", "test-tmpl", tc.launchType, "test-pool", tc.podCondition).Inc()

			if testutil.CollectAndCount(SandboxClaimCreationTotal) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestBuildInfo(t *testing.T) {
	expected := strings.TrimSpace(`
		# HELP agent_sandbox_build_info Agent sandbox controller build metadata exposed as labels with a constant value of 1.
		# TYPE agent_sandbox_build_info gauge
		agent_sandbox_build_info{build_date="`+version.Get().BuildDate+`",compiler="`+version.Get().Compiler+`",git_commit="`+version.Get().GitSHA+`",git_version="`+version.Get().GitVersion+`",go_version="`+version.Get().GoVersion+`",platform="`+version.Get().Platform+`"} 1
	`) + "\n"

	if err := testutil.CollectAndCompare(BuildInfo, strings.NewReader(expected)); err != nil {
		t.Errorf("BuildInfo metric mismatch: %v", err)
	}
}
