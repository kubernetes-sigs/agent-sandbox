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

package controllers

// Status-write reduction for the Sandbox launch path.
//
// A healthy launch used to cost ~2.3 status writes (an initial "not Ready
// yet" fill, sometimes an intermediate reason change, then the Ready flip)
// plus ~0.8 no-op PATCH requests re-issued by reconciles racing the
// controller's own informer (measured on a 100-node/50k-sandbox run:
// 107k status PATCH requests and ~39k no-ops for 50k launches). Nothing
// reads the transitional writes on the fast path -- consumers (the claim
// controller, kubectl wait) act on Ready -- so this file provides:
//
//  1. materialStatusChange: an explicit classification of status changes
//     into material (must write now) vs transitional (may ride along with
//     the next material write).
//  2. Creation-age gating (see updateStatus): with
//     --sandbox-transitional-status-window=T, a transitional change on a
//     sandbox younger than T is skipped and requeued to flush at age T. A
//     launch that reaches Ready inside T writes status exactly once; a
//     sandbox that is stuck still converges to an explanatory status at T.
//     Like the write-behind window (writebehind_requeue.go), the deferred
//     write is recomputed from informer state by the flushing pass, so a
//     crash can never lose a mutation.
//  3. staleCacheGuard: suppresses whole reconcile passes that run against
//     an informer cache that has not yet caught up with this controller's
//     own last sandbox write -- the source of the no-op PATCHes. The
//     guard stores only resourceVersions, never payloads.

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// terminalReadyReasons are Ready-condition reasons that carry terminal or
// operator-actionable meaning even without a condition Status flip: a
// never-Ready sandbox that expires stays Ready=False, but the expiry flow
// (handleSandboxExpiry) only proceeds once the Expired mark is persisted,
// and Succeeded/Failed are ends of the lifecycle, not launch progress.
var terminalReadyReasons = map[string]bool{
	sandboxv1beta1.SandboxReasonExpired:      true,
	sandboxv1beta1.SandboxReasonPodFailed:    true,
	sandboxv1beta1.SandboxReasonPodSucceeded: true,
}

// materialStatusChange reports whether the old->new status change must be
// written immediately (material) rather than deferred (transitional).
//
// Material changes are the ones consumers act on:
//   - any condition's Status VALUE changing (False->True, True->False,
//     anything<->Unknown) -- this covers the Ready flip in both directions,
//     Suspended taking effect, and Finished appearing (it is created with
//     Status=True, so its very addition is material);
//   - a condition disappearing (Finished is removed when the pod restarts);
//   - a condition appearing with any Status other than False (the initial
//     Ready=False / Suspended=False fill is launch progress, not news);
//   - the Ready reason moving to or from a terminal reason (see
//     terminalReadyReasons).
//
// Everything else is transitional: reason/message churn while the Status
// value holds (Pending -> ContainerCreating), observedGeneration bumps, and
// non-condition field fills (nodeName, podIPs, serviceFQDN, selector) --
// all of which ride along with the next material write, where consumers
// actually read them.
func materialStatusChange(oldStatus, newStatus *sandboxv1beta1.SandboxStatus) bool {
	oldConds := make(map[string]*metav1.Condition, len(oldStatus.Conditions))
	for i := range oldStatus.Conditions {
		oldConds[oldStatus.Conditions[i].Type] = &oldStatus.Conditions[i]
	}
	newSeen := make(map[string]bool, len(newStatus.Conditions))
	for i := range newStatus.Conditions {
		nc := &newStatus.Conditions[i]
		newSeen[nc.Type] = true
		oc, existed := oldConds[nc.Type]
		if !existed {
			if nc.Status != metav1.ConditionFalse {
				return true
			}
			if nc.Type == string(sandboxv1beta1.SandboxConditionReady) && terminalReadyReasons[nc.Reason] {
				return true
			}
			continue
		}
		if oc.Status != nc.Status {
			return true
		}
		if nc.Type == string(sandboxv1beta1.SandboxConditionReady) && oc.Reason != nc.Reason &&
			(terminalReadyReasons[oc.Reason] || terminalReadyReasons[nc.Reason]) {
			return true
		}
	}
	for condType := range oldConds {
		if !newSeen[condType] {
			return true
		}
	}
	return false
}

// rvPair is one recorded sandbox write: the resourceVersion the write was
// issued against and the resourceVersion it produced.
type rvPair struct {
	before, after string
}

// staleCacheGuard remembers, per sandbox, the resourceVersions bracketing
// this controller's most recent write to that sandbox (main resource or
// status). A reconcile pass whose informer copy still shows the pre-write
// resourceVersion is running on state this controller has already
// superseded: acting on it can only re-derive writes the apiserver will
// no-op (measured: ~19% of all PATCH requests under launch load). Such a
// pass is skipped outright -- no requeue is needed, because the recorded
// write itself guarantees a future watch event (and therefore a fresh
// reconcile) for the object.
//
// Correctness rests on resourceVersion EQUALITY only (never ordering): the
// informer replays one object's versions in order, so observing the
// pre-write version means the post-write event has not been delivered yet,
// and observing anything else means it has (or a later writer has moved the
// object on, which is equally fresh for our purposes). Writes that do not
// change the resourceVersion (a patch the apiserver no-ops) are never
// recorded -- recording before==after would classify every future pass as
// stale with no event ever coming to clear it.
//
// Like deferredWriteClock, the map holds no mutation payload; losing it on
// crash or failover costs at most one redundant no-op PATCH per object.
type staleCacheGuard struct {
	mu sync.Mutex
	m  map[types.NamespacedName]rvPair
}

// record notes a successful write that moved key from rvBefore to rvAfter.
// No-op writes (rvAfter empty or equal to rvBefore) are ignored.
func (g *staleCacheGuard) record(key types.NamespacedName, rvBefore, rvAfter string) {
	if rvAfter == "" || rvAfter == rvBefore {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = make(map[types.NamespacedName]rvPair)
	}
	g.m[key] = rvPair{before: rvBefore, after: rvAfter}
}

// stillStale reports whether a pass observing observedRV for key is running
// behind this controller's own last write. Any observation other than the
// recorded pre-write version proves the cache has caught up (or moved
// further) and drops the record.
func (g *staleCacheGuard) stillStale(key types.NamespacedName, observedRV string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	pair, ok := g.m[key]
	if !ok {
		return false
	}
	if observedRV == pair.before {
		return true
	}
	delete(g.m, key)
	return false
}

// clear drops the record for key, if any (object deleted or gone).
func (g *staleCacheGuard) clear(key types.NamespacedName) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, key)
}
