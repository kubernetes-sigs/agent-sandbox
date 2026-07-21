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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestBuildManagerOptionsReleasesLeaseOnCancel pins the graceful-shutdown
// behavior: the manager must release its leader-election Lease when it stops
// cleanly, so a rolling update hands leadership over immediately instead of
// waiting out the LeaseDuration.
func TestBuildManagerOptionsReleasesLeaseOnCancel(t *testing.T) {
	opts := buildManagerOptions(runtime.NewScheme(), metricsserver.Options{}, ":8081", true, "agent-sandbox-system")
	assert.True(t, opts.LeaderElectionReleaseOnCancel,
		"LeaderElectionReleaseOnCancel must stay true so graceful shutdowns hand over leadership without waiting out the LeaseDuration")
}

// TestBuildManagerOptionsLeaderElectionID pins the leader-election lock name;
// changing it would let two controller versions run as leaders concurrently
// during an upgrade.
func TestBuildManagerOptionsLeaderElectionID(t *testing.T) {
	opts := buildManagerOptions(runtime.NewScheme(), metricsserver.Options{}, ":8081", true, "")
	assert.Equal(t, "a3317529.agent-sandbox.x-k8s.io", opts.LeaderElectionID)
}

// TestBuildManagerOptionsPassThrough verifies the flag-derived inputs are
// forwarded unchanged into the manager options.
func TestBuildManagerOptionsPassThrough(t *testing.T) {
	scheme := runtime.NewScheme()
	metricsOpts := metricsserver.Options{BindAddress: ":8080"}

	for _, enableLeaderElection := range []bool{true, false} {
		for _, namespace := range []string{"", "agent-sandbox-system"} {
			opts := buildManagerOptions(scheme, metricsOpts, ":8081", enableLeaderElection, namespace)
			assert.Equal(t, enableLeaderElection, opts.LeaderElection,
				"LeaderElection must pass through --leader-elect")
			assert.Equal(t, namespace, opts.LeaderElectionNamespace,
				"LeaderElectionNamespace must pass through --leader-election-namespace")
			assert.Same(t, scheme, opts.Scheme)
			assert.Equal(t, ":8081", opts.HealthProbeBindAddress)
			assert.Equal(t, ":8080", opts.Metrics.BindAddress)
		}
	}
}
