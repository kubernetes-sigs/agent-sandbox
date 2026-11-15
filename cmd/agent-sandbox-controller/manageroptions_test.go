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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var defaultTiming = leaderElectionTiming{
	LeaseDuration: defaultLeaderElectionLeaseDuration,
	RenewDeadline: defaultLeaderElectionRenewDeadline,
	RetryPeriod:   defaultLeaderElectionRetryPeriod,
}

// TestBuildManagerOptionsReleasesLeaseOnCancel pins the graceful-shutdown
// behavior: the manager must release its leader-election Lease when it stops
// cleanly, so a rolling update hands leadership over immediately instead of
// waiting out the LeaseDuration.
func TestBuildManagerOptionsReleasesLeaseOnCancel(t *testing.T) {
	opts := buildManagerOptions(runtime.NewScheme(), metricsserver.Options{}, ":8081", true, "agent-sandbox-system", defaultTiming)
	assert.True(t, opts.LeaderElectionReleaseOnCancel,
		"LeaderElectionReleaseOnCancel must stay true so graceful shutdowns hand over leadership without waiting out the LeaseDuration")
}

// TestBuildManagerOptionsLeaderElectionID pins the leader-election lock name;
// changing it would let two controller versions run as leaders concurrently
// during an upgrade.
func TestBuildManagerOptionsLeaderElectionID(t *testing.T) {
	opts := buildManagerOptions(runtime.NewScheme(), metricsserver.Options{}, ":8081", true, "", defaultTiming)
	assert.Equal(t, "a3317529.agent-sandbox.x-k8s.io", opts.LeaderElectionID)
}

// TestBuildManagerOptionsPassThrough verifies the flag-derived inputs are
// forwarded unchanged into the manager options.
func TestBuildManagerOptionsPassThrough(t *testing.T) {
	scheme := runtime.NewScheme()
	metricsOpts := metricsserver.Options{BindAddress: ":8080"}

	for _, enableLeaderElection := range []bool{true, false} {
		for _, namespace := range []string{"", "agent-sandbox-system"} {
			opts := buildManagerOptions(scheme, metricsOpts, ":8081", enableLeaderElection, namespace, defaultTiming)
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

// TestBuildManagerOptionsDefaultLeaderElectionTiming pins the flag defaults to
// the controller-runtime defaults, so a deployment that does not set the
// leader-election timing flags keeps the same Lease behavior.
func TestBuildManagerOptionsDefaultLeaderElectionTiming(t *testing.T) {
	opts := buildManagerOptions(runtime.NewScheme(), metricsserver.Options{}, ":8081", true, "", defaultTiming)
	require.NotNil(t, opts.LeaseDuration)
	require.NotNil(t, opts.RenewDeadline)
	require.NotNil(t, opts.RetryPeriod)
	assert.Equal(t, 15*time.Second, *opts.LeaseDuration)
	assert.Equal(t, 10*time.Second, *opts.RenewDeadline)
	assert.Equal(t, 2*time.Second, *opts.RetryPeriod)
}

// TestBuildManagerOptionsLeaderElectionTiming verifies that the
// leader-election timing flags reach the manager options unchanged.
func TestBuildManagerOptionsLeaderElectionTiming(t *testing.T) {
	timing := leaderElectionTiming{
		LeaseDuration: 60 * time.Second,
		RenewDeadline: 40 * time.Second,
		RetryPeriod:   5 * time.Second,
	}
	opts := buildManagerOptions(runtime.NewScheme(), metricsserver.Options{}, ":8081", true, "", timing)
	assert.Equal(t, 60*time.Second, *opts.LeaseDuration)
	assert.Equal(t, 40*time.Second, *opts.RenewDeadline)
	assert.Equal(t, 5*time.Second, *opts.RetryPeriod)
}

func TestLeaderElectionTimingValidate(t *testing.T) {
	tests := []struct {
		name    string
		timing  leaderElectionTiming
		wantErr string
		// stricterThanClientGo marks inputs that client-go accepts but
		// validate() rejects, because the lease duration stored in the Lease
		// in whole seconds would not exceed the renew deadline.
		stricterThanClientGo bool
	}{
		{name: "defaults", timing: defaultTiming},
		{name: "longer timings for a slow API server", timing: leaderElectionTiming{LeaseDuration: 60 * time.Second, RenewDeadline: 40 * time.Second, RetryPeriod: 5 * time.Second}},
		{name: "zero lease duration", timing: leaderElectionTiming{LeaseDuration: 0, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}, wantErr: "--leader-elect-lease-duration must be greater than zero"},
		{name: "negative renew deadline", timing: leaderElectionTiming{LeaseDuration: 15 * time.Second, RenewDeadline: -time.Second, RetryPeriod: 2 * time.Second}, wantErr: "--leader-elect-renew-deadline must be greater than zero"},
		{name: "zero retry period", timing: leaderElectionTiming{LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 0}, wantErr: "--leader-elect-retry-period must be greater than zero"},
		{name: "lease duration equal to renew deadline", timing: leaderElectionTiming{LeaseDuration: 10 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}, wantErr: "must be greater than --leader-elect-renew-deadline"},
		{name: "fractional lease duration above renew deadline in whole seconds", timing: leaderElectionTiming{LeaseDuration: 15500 * time.Millisecond, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}},
		{name: "lease duration not above renew deadline in whole seconds", timing: leaderElectionTiming{LeaseDuration: 1500 * time.Millisecond, RenewDeadline: 1400 * time.Millisecond, RetryPeriod: 100 * time.Millisecond}, wantErr: "rounded down to whole seconds", stricterThanClientGo: true},
		{name: "lease duration rounds down to whole renew deadline", timing: leaderElectionTiming{LeaseDuration: 10900 * time.Millisecond, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}, wantErr: "rounded down to whole seconds", stricterThanClientGo: true},
		{name: "sub-second lease duration", timing: leaderElectionTiming{LeaseDuration: 900 * time.Millisecond, RenewDeadline: 500 * time.Millisecond, RetryPeriod: 100 * time.Millisecond}, wantErr: "rounded down to whole seconds", stricterThanClientGo: true},
		{name: "renew deadline equal to jittered retry period", timing: leaderElectionTiming{LeaseDuration: 15 * time.Second, RenewDeadline: 12 * time.Second, RetryPeriod: 10 * time.Second}, wantErr: "x --leader-elect-retry-period"},
		{name: "renew deadline just above jittered retry period", timing: leaderElectionTiming{LeaseDuration: 15 * time.Second, RenewDeadline: 12*time.Second + time.Nanosecond, RetryPeriod: 10 * time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.timing.validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}

			// The validation must reject everything client-go rejects, so a
			// value that passes at startup cannot fail later when the manager
			// starts leader election. Apart from the whole-second Lease rule,
			// it must also accept everything client-go accepts.
			_, clientGoErr := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
				Lock:          &resourcelock.LeaseLock{LockConfig: resourcelock.ResourceLockConfig{Identity: "test"}},
				LeaseDuration: tc.timing.LeaseDuration,
				RenewDeadline: tc.timing.RenewDeadline,
				RetryPeriod:   tc.timing.RetryPeriod,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(context.Context) {},
					OnStoppedLeading: func() {},
				},
			})
			if tc.stricterThanClientGo {
				require.NoError(t, clientGoErr, "client-go now rejects this input; drop stricterThanClientGo")
				return
			}
			assert.Equal(t, err == nil, clientGoErr == nil,
				"validate() and client-go disagree: validate=%v client-go=%v", err, clientGoErr)
		})
	}
}
