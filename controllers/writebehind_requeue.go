// Copyright 2025 The Kubernetes Authors.
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

// RequeueAfter-based write deferral, per the review suggestion that a
// controller "can also return with RequeueAfter ... and get some of this for
// free". Instead of holding a pending mutation in memory, the reconciler
// skips the recoverable write and asks the workqueue to redeliver the
// request once the coalescing window has elapsed; the redelivered pass
// recomputes the desired state from informer state and writes once. The
// workqueue is the timer and its per-key dedup is the coalescing.

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// deferredWriteClock remembers, per reconcile request, WHEN the currently
// pending recoverable-write deferral was first observed.
//
// This timestamp map is the one piece of in-memory state the mechanism cannot
// avoid: "write once the window has elapsed" needs a first-seen clock, the
// object carries no such clock in its own state, and stamping one onto the
// object would itself be a write — the very thing being deferred. Re-arming
// the window on every event instead (no state at all) would starve the write
// under continuous redelivery.
//
// Crucially the map stores NO mutation payload: the deferred write is always
// recomputed from informer state by the pass that flushes it. Losing the map
// in a crash or failover therefore only restarts a (sub-second) deferral
// window on the replacement leader's first pass — it can never lose a
// mutation. Entries are dropped whenever a pass has nothing pending, and on
// object deletion.
type deferredWriteClock struct {
	mu    sync.Mutex
	first map[types.NamespacedName]time.Time

	// now is overridable in tests; nil means time.Now.
	now func() time.Time
}

// observe registers (or re-reads) the pending deferral for key and reports
// whether its window has elapsed (due) and, if not, how long remains.
func (c *deferredWriteClock) observe(key types.NamespacedName, window time.Duration) (due bool, wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nowFn := c.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	if c.first == nil {
		c.first = make(map[types.NamespacedName]time.Time)
	}
	firstSeen, ok := c.first[key]
	if !ok {
		c.first[key] = now
		return false, window
	}
	deadline := firstSeen.Add(window)
	if !now.Before(deadline) {
		return true, 0
	}
	return false, deadline.Sub(now)
}

// clear drops the pending deferral for key, if any.
func (c *deferredWriteClock) clear(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.first, key)
}

// writeDeferral is the per-reconcile-pass view of the deferral clock for one
// request. Write sites call shouldWrite; Reconcile's tail turns a deferred
// pass into ctrl.Result{RequeueAfter: wait} and clears the clock entry when
// nothing is pending.
type writeDeferral struct {
	clock  *deferredWriteClock
	key    types.NamespacedName
	window time.Duration

	// deferred reports whether any write site skipped its write this pass.
	deferred bool
	// wait is the shortest remaining window across deferred sites this pass.
	wait time.Duration
}

// shouldWrite reports whether the calling site must issue its write NOW
// (the deferral window for this request has elapsed) or skip it (the write
// is deferred to the requeued pass).
func (d *writeDeferral) shouldWrite() bool {
	due, wait := d.clock.observe(d.key, d.window)
	if due {
		return true
	}
	d.deferred = true
	if d.wait == 0 || wait < d.wait {
		d.wait = wait
	}
	return false
}
