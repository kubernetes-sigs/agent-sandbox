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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/leaderelection"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// The leader-election timing defaults match the controller-runtime defaults,
// so leaving the flags unset keeps the previous behavior.
const (
	defaultLeaderElectionLeaseDuration = 15 * time.Second
	defaultLeaderElectionRenewDeadline = 10 * time.Second
	defaultLeaderElectionRetryPeriod   = 2 * time.Second
)

// leaderElectionTiming holds the leader-election Lease timings. Clusters
// whose API server or etcd is slow at times can raise them so that the leader
// keeps its Lease through a slow renewal instead of exiting.
type leaderElectionTiming struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// validate applies the same rules as client-go's leaderelection.NewLeaderElector,
// so that a bad combination fails at startup with the flag names in the error
// instead of when the manager starts leader election.
func (t leaderElectionTiming) validate() error {
	if t.LeaseDuration <= 0 {
		return fmt.Errorf("--leader-elect-lease-duration must be greater than zero, got %s", t.LeaseDuration)
	}
	if t.RenewDeadline <= 0 {
		return fmt.Errorf("--leader-elect-renew-deadline must be greater than zero, got %s", t.RenewDeadline)
	}
	if t.RetryPeriod <= 0 {
		return fmt.Errorf("--leader-elect-retry-period must be greater than zero, got %s", t.RetryPeriod)
	}
	if t.LeaseDuration <= t.RenewDeadline {
		return fmt.Errorf("--leader-elect-lease-duration (%s) must be greater than --leader-elect-renew-deadline (%s)",
			t.LeaseDuration, t.RenewDeadline)
	}
	if t.RenewDeadline <= time.Duration(leaderelection.JitterFactor*float64(t.RetryPeriod)) {
		return fmt.Errorf("--leader-elect-renew-deadline (%s) must be greater than %.1f x --leader-elect-retry-period (%s)",
			t.RenewDeadline, leaderelection.JitterFactor, t.RetryPeriod)
	}
	return nil
}

func buildManagerOptions(scheme *runtime.Scheme, metricsOpts metricsserver.Options, probeAddr string, enableLeaderElection bool, leaderElectionNamespace string, timing leaderElectionTiming) ctrl.Options {
	return ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOpts,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionNamespace: leaderElectionNamespace,
		LeaderElectionID:        "a3317529.agent-sandbox.x-k8s.io",
		LeaseDuration:           &timing.LeaseDuration,
		RenewDeadline:           &timing.RenewDeadline,
		RetryPeriod:             &timing.RetryPeriod,
		// Release the leader Lease on graceful shutdown so a rolling update
		// hands over leadership in ~0-2s instead of waiting out the full
		// LeaseDuration (15s by default) with no active controller — at a sustained 500
		// claims/s that gap is ~7,500 claims queueing during a routine
		// deploy. Crash failover still pays lease expiry by design
		// (split-brain safety); only clean exits release early. Safe here:
		// controller-runtime requires the binary to exit promptly once the
		// manager stops; mgr.Start is the last explicit statement in main(),
		// and any deferred shutdown work (e.g. tracing cleanup) must stay
		// bounded so the process still exits promptly.
		LeaderElectionReleaseOnCancel: true,
	}
}
