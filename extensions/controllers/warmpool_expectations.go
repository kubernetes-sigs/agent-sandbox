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

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// expectationsTimeout is how long recorded expectations are trusted before the
// reconciler falls back to the informer cache. Mirrors the ReplicaSet
// controller's ExpectationsTimeout: long enough for a watch event to arrive
// under heavy apiserver load, short enough that a lost event cannot wedge a
// pool forever.
//
// The trade, explicitly: while an expectation is outstanding the pool can
// neither create nor delete-excess, so a lost or badly delayed watch event
// can idle a pool for up to this long (the 30s fallback requeue only
// re-checks satisfaction, it does not shorten this window). At scale that can
// surface as a ~5m warm-fill p99 outlier. This is conservative by design:
// under-creation for a few minutes is recoverable, while trusting a stale
// cache too early re-opens the #1215 over-creation runaway. If warm pools
// (high churn, normally fast event delivery) ever prove to deserve a shorter
// timeout than kube's, this constant is the single knob.
const expectationsTimeout = 5 * time.Minute

// A same-name recreated pool must not inherit outstanding writes.
type warmPoolIdentity struct {
	types.NamespacedName
	UID types.UID
}

// warmPoolExpectations tracks in-flight Sandbox creations and deletions per
// SandboxWarmPool, closing the read-after-write gap between the writes a
// reconcile issues and the informer cache those writes are later read back
// from (#1215).
//
// Without this, rapid re-reconciles of the same pool (each self-issued create
// enqueues the pool again via the ownership watch) each recompute
// "desired - len(cachedSandboxes)" against a cache that does not yet contain
// the previous reconcile's creates, and every pass creates toward the target
// again — ~10x over-creation observed at high worker concurrency.
//
// This is a self-contained analog of k8s.io/kubernetes/pkg/controller's
// UIDTrackingControllerExpectations (which is internal to kube and not
// importable here):
//
//   - Creations are operation-tracked: tokens are registered before POST and
//     acknowledged only by matching observations or confirmed failed writes.
//     Repeated events cannot consume another operation's credit.
//   - Deletions are UID-tracked: recorded before each delete, cleared when the
//     watch observes the delete event. UID tracking also lets the reconciler
//     treat a sandbox it already deleted — but which the stale cache still
//     lists as live — as terminating rather than active.
//   - Expectations expire after expectationsTimeout as a conservative fallback
//     for lost watch events; expiry re-enables reconciliation from cache
//     state.
//
// All methods are safe for concurrent use.
type warmPoolExpectations struct {
	// now is a test hook; defaults to time.Now.
	now func() time.Time

	mu    sync.Mutex
	pools map[warmPoolIdentity]*poolExpectation
}

// poolExpectation is the outstanding expectation record for one pool.
type poolExpectation struct {
	pendingCreations map[string]struct{}
	pendingDeletions map[types.UID]struct{}
	// raisedAt is refreshed every time new expectations are recorded; the
	// whole record expires expectationsTimeout after the last raise.
	raisedAt time.Time
}

func newWarmPoolExpectations() *warmPoolExpectations {
	return &warmPoolExpectations{
		now:   time.Now,
		pools: make(map[warmPoolIdentity]*poolExpectation),
	}
}

// satisfiedLocked reports whether the pool has no live expectations, garbage
// collecting fulfilled or expired records. Callers must hold e.mu.
func (e *warmPoolExpectations) satisfiedLocked(key warmPoolIdentity) bool {
	entry, ok := e.pools[key]
	if !ok {
		return true
	}
	if len(entry.pendingCreations) == 0 && len(entry.pendingDeletions) == 0 {
		delete(e.pools, key)
		return true
	}
	if e.now().Sub(entry.raisedAt) > expectationsTimeout {
		// Conservative fallback: a watch event was lost or the apiserver is
		// severely degraded. Trust the cache again rather than wedging the
		// pool forever.
		delete(e.pools, key)
		return true
	}
	return false
}

// SatisfiedExpectations reports whether all previously recorded creations and
// deletions for the pool have been observed (or have timed out). The
// reconciler must not issue new creates or excess deletes while this is false.
func (e *warmPoolExpectations) SatisfiedExpectations(key warmPoolIdentity) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.satisfiedLocked(key)
}

// TryExpectCreations atomically checks that the pool's expectations are
// satisfied and, if so, records the operation tokens, returning true. If
// expectations are still outstanding it records nothing and returns false.
// The check-and-record is a single critical section so that even overlapping
// reconciles of the same pool cannot both pass the gate.
func (e *warmPoolExpectations) TryExpectCreations(key warmPoolIdentity, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	pending := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token == "" {
			return false
		}
		pending[token] = struct{}{}
	}
	if len(pending) != len(tokens) {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.satisfiedLocked(key) {
		return false
	}
	e.pools[key] = &poolExpectation{
		pendingCreations: pending,
		pendingDeletions: make(map[types.UID]struct{}),
		raisedAt:         e.now(),
	}
	return true
}

// CreationObserved also cancels a token for an operation known not to have
// happened. Unknown or duplicate observations cannot acknowledge another POST.
func (e *warmPoolExpectations) CreationObserved(key warmPoolIdentity, token string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.pools[key]
	if !ok {
		return
	}
	delete(entry.pendingCreations, token)
	if len(entry.pendingCreations) == 0 && len(entry.pendingDeletions) == 0 {
		delete(e.pools, key)
	}
}

// ExpectDeletion records that the sandbox with the given UID is about to be
// deleted by this controller.
//
// It MUST be recorded before the delete is issued: the watch delete event can
// be delivered on the informer goroutine while the DELETE call is still in
// flight (or immediately after it returns), and the handler's
// DeletionObserved for an expectation that does not exist yet is a no-op —
// recording afterwards would then leave an expectation nothing will ever
// lower, wedging the pool until the timeout. The caller cancels with a
// synthetic DeletionObserved if the delete fails (nothing was deleted, no
// event will come) — the same record-then-act protocol as the ReplicaSet
// controller (ExpectDeletions before podControl.DeletePod, DeletionObserved
// on delete error "because the informer won't observe this deletion").
//
// Empty UIDs are ignored: they cannot be matched to a watch delete event and
// would collide with each other in the pending set.
func (e *warmPoolExpectations) ExpectDeletion(key warmPoolIdentity, uid types.UID) {
	if uid == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.pools[key]
	if !ok {
		entry = &poolExpectation{pendingDeletions: make(map[types.UID]struct{})}
		e.pools[key] = entry
	}
	entry.pendingDeletions[uid] = struct{}{}
	entry.raisedAt = e.now()
}

// DeletionObserved clears a pending deletion. Called by the watch handler when
// the delete event for the UID is observed, and by the reconciler to roll back
// an expectation whose delete call failed.
func (e *warmPoolExpectations) DeletionObserved(key warmPoolIdentity, uid types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.pools[key]
	if !ok {
		return
	}
	delete(entry.pendingDeletions, uid)
	if len(entry.pendingCreations) == 0 && len(entry.pendingDeletions) == 0 {
		delete(e.pools, key)
	}
}

// IsPendingDeletion reports whether the controller has issued a not yet
// watch-observed delete for the given UID. Sandboxes in this state still
// occupy capacity and must be counted as terminating, not active.
func (e *warmPoolExpectations) IsPendingDeletion(key warmPoolIdentity, uid types.UID) bool {
	if uid == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.pools[key]
	if !ok {
		return false
	}
	if e.now().Sub(entry.raisedAt) > expectationsTimeout {
		return false
	}
	_, pending := entry.pendingDeletions[uid]
	return pending
}

// Forget drops all expectation state for a pool. Called when the pool is
// deleted.
func (e *warmPoolExpectations) Forget(key types.NamespacedName) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for identity := range e.pools {
		if identity.NamespacedName == key {
			delete(e.pools, identity)
		}
	}
}
