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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// buildManagerOptions constructs the controller manager options used by
// main(). The webhook server option is applied separately in main() when the
// webhook subsystem is enabled.
func buildManagerOptions(scheme *runtime.Scheme, metricsOpts metricsserver.Options, probeAddr string, enableLeaderElection bool, leaderElectionNamespace string) ctrl.Options {
	return ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOpts,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionNamespace: leaderElectionNamespace,
		LeaderElectionID:        "a3317529.agent-sandbox.x-k8s.io",
		// Release the leader Lease on graceful shutdown so a rolling update
		// hands over leadership in ~0-2s instead of waiting out the full 15s
		// LeaseDuration with no active controller — at a sustained 500
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
