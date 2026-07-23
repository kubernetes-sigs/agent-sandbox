# fix(warmpool): stop over-creating replicas — expectations tracking, terminating-aware create gating, unschedulable-aware stuck GC (#1215)

Fixes #1215

## What happened (RCA)

Credit to @tomergee for the log-confirmed root-cause analysis on the issue: for
500 pools with `replicas: 1`, the runaway window showed **>=5,000 "Creating new
pool sandboxes" events but only 1 stuck-sandbox delete** — the over-creation is
on the **create path**, not the grace-period GC loop.

`reconcilePool` computes

```go
toCreate = desiredReplicas - len(activeSandboxes)   // activeSandboxes from the informer cache
```

but the informer cache **lags the controller's own just-issued creates**. Every
create event re-enqueues the pool via the ownership watch, so the pool is
re-reconciled while the cache still shows the pre-create state, and each pass
creates toward the target again. High `--sandbox-warm-pool-concurrent-workers`
(1000 in the report) amplifies the rate; the result was ~10x over-creation
(~8,400 pods for a 500-pod target), snowballing into a cluster-wide
scheduler/apiserver overload. This is the classic read-after-write gap that the
ReplicaSet controller closes with `UIDTrackingControllerExpectations`.

Secondary defects fixed in the same change (all called out on the issue):

1. **Terminating sandboxes didn't count against the target.** Anything with a
   deletion timestamp was excluded entirely, so creates could outrun deletes
   and balloon the population while deletes drained (delete-lag ballooning).
2. **The 5-minute stuck-sandbox GC deletes unschedulable sandboxes.** Under a
   capacity shortfall the replacement is equally unschedulable, producing an
   unbounded delete->create loop.
3. **No visible signal when a pool can't progress** — a capacity shortfall
   churned silently instead of degrading visibly.

## The fix

### 1. Per-pool expectations tracker (`extensions/controllers/warmpool_expectations.go`)

A self-contained analog of kube's `UIDTrackingControllerExpectations`
(intentionally not vendored — it's internal to k8s.io/kubernetes):

- **Creations are counted**: recorded *before* creates are issued
  (`TryExpectCreations`), lowered when the watch observes the resulting add
  events, or immediately for creates that failed (no event will ever come).
- **Deletions are UID-tracked**: recorded before each delete, cleared on the
  watch delete event. UID tracking also lets the reconciler treat a sandbox it
  already deleted — but which the stale cache still lists as live — as
  terminating rather than active.
- **Conservative 5-minute timeout** (mirrors kube's `ExpectationsTimeout`): a
  lost watch event can never wedge a pool; expiry falls back to trusting the
  cache.
- `TryExpectCreations` is an **atomic check-and-record**, so even overlapping
  reconciles of the same pool cannot both pass the create gate.

The ownership watch (`Owns(&Sandbox{})`) is replaced by an equivalent
`Watches` with a wrapping handler (`warmPoolSandboxEventHandler`) that lowers
expectations on owned add/delete events *before* enqueueing the pool, so a
reconcile triggered by our own write always sees the expectation already
lowered.

The reconciler refuses to create (and to delete "excess", which is equally
computed from the cached list) while expectations are unsatisfied, with a 30s
fallback requeue for lost events.

### 2. Hard create invariant: population, not just active count

Creates are gated on the pool's whole live population:

```
active (incl. non-Ready) + terminating-still-present (deletionTimestamp set, or deleted-by-us but not yet observed)
```

A pool can no longer create while `population >= spec.replicas`, regardless of
readiness or delete lag. Terminating sandboxes remain excluded from
`status.replicas` / `status.readyReplicas` accounting. A consequence:
delete-and-replace (stuck GC, `Recreate` rollouts) now replaces on the *next*
reconcile, after the deletion is observed — one watch round-trip of latency in
exchange for a population that can never overshoot.

### 3. Unschedulable-aware stuck GC

Before delete-and-replacing a non-Ready sandbox past the readiness grace
period, the reconciler checks the backing pod's `PodScheduled` condition. If
the pod is `Unschedulable`, the sandbox is **held** (kept, counted, not
replaced) with a rate-limited 1-minute requeue. Genuinely stuck sandboxes (pod
scheduled or missing) are still replaced exactly as before.

### 4. Self-scheduled post-grace evaluation (also fixes a latent upstream reliability defect)

Live-repro forensics found a reachability gap: in a **quiet cluster**, a pool
whose sandboxes settle at Ready=False receives **no further reconciles** —
pod `FailedScheduling` events never touch the Sandbox objects, and the next
guaranteed reconcile is the ~10h cache resync. That means upstream's 5-minute
stuck-sandbox GC never actually fires at its deadline without ambient traffic
(a latent upstream defect), and by extension the unschedulable hold and the
NotProgressing signal would be unreachable too.

Fix: whenever a reconcile observes not-yet-Ready sandboxes still inside the
grace period, it returns `RequeueAfter = time until the earliest grace
deadline (+2s slack)`, composed with any other pending requeue by taking the
minimum. The post-grace evaluation (stuck GC, unschedulable hold,
NotProgressing) is now deterministic and self-driving.

### 5. Not-progressing signal

When a pool is holding unschedulable sandboxes it emits a
`WarmPoolNotProgressing` **Warning Event** on the SandboxWarmPool (once per
state transition, not per reconcile), and a `WarmPoolProgressing` Normal Event
when progress resumes.

Note: `SandboxWarmPoolStatus` has no `conditions` field today; adding one is a
CRD schema change and deliberately out of scope for this fix. If maintainers
want a `WarmPoolNotProgressing` status *condition* as well, that's a small
additive follow-up (new status field + condition constant). Events keep this
change schema-neutral.

No CRD or RBAC manifest changes: the extensions ClusterRole already grants
`pods get/list/watch` and `events` create/patch/update (the new kubebuilder
markers on the warm-pool reconciler are strict subsets of existing grants).

## Test coverage

- `warmpool_expectations_test.go` — tracker unit tests: record/observe
  lifecycle for creates and UID-tracked deletes, failed-create lowering with
  clamping, timeout expiry + refresh-on-raise, `Forget`, atomicity of the gate
  under 64 concurrent raisers, and a mixed-ops hammer (all `-race`).
- `sandboxwarmpool_overcreation_test.go` — regression tests built around a
  `laggingClient` wrapper whose `List` hides the controller's own creates
  until `catchUp()` (the exact stale-cache window from the issue):
  - **Stale-cache repro**: 5 reconciles against the lagging list → creates ==
    `spec.replicas` exactly; then watch-observation via the real event handler
    and steady-state reconciles → still no extra creates. *Repro proof:* with
    the expectations gate disabled (forcing `TryExpectCreations` → true), this
    test fails with **15 creates for replicas=3** — the original bug shape.
  - **Concurrent reconciles** (8 goroutines, `-race`): no over-creation
    (24 creates for replicas=3 without the gate).
  - **Terminating counts against the target**: a pool-owned sandbox with a
    deletion timestamp (finalizer-pinned) suppresses the replacement create
    until it is fully gone.
  - **Unschedulable GC**: held (not deleted), no replacement, rate-limited
    requeue, `WarmPoolNotProgressing` Warning emitted exactly once, and
    `WarmPoolProgressing` emitted when the sandbox goes Ready; genuinely stuck
    and pod-missing sandboxes are still replaced.
  - **Self-scheduled grace requeue** (fake clock, exact asserts): young
    not-Ready sandboxes arm `RequeueAfter` = earliest remaining grace (+slack);
    a settled Ready pool arms nothing; and a full quiet-cluster walk-through —
    reconcile inside grace, advance the clock by exactly the returned requeue
    with NO external events, and the unschedulable hold plus exactly one
    `WarmPoolNotProgressing` event fire deterministically.
  - Watch-handler bookkeeping: owned add/delete events lower expectations;
    orphan/foreign-owned events are ignored.
- Existing suite updated where semantics deliberately changed (stuck GC and
  `Recreate`/templateRef rollouts now replace on the pass after the deletion
  is observed; comments in each test explain why).

`go build ./...`, `go vet ./...`, `gofmt`, `go test <all non-e2e packages>
-race -count=1`, and `make lint-go` are all clean.

## Live reproduction evidence

_Placeholder — before/after kops repro to be attached by the coordinator:_

- **Before (main):** N pools x replicas under cache lag / worker concurrency →
  create-event count vs `Σ spec.replicas`, pod count over time.
- **After (this branch):** same scenario → total creates == `Σ spec.replicas`,
  no delete->create churn on unschedulable pools, `WarmPoolNotProgressing`
  events visible in `kubectl describe swp`.
