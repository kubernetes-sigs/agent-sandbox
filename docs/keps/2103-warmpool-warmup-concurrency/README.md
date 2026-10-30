# KEP-2103: SandboxWarmPool Warmup Concurrency Strategy

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Why the existing shaping controls do not solve this](#why-the-existing-shaping-controls-do-not-solve-this)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: a pool of pre-built development environments](#story-1-a-pool-of-pre-built-development-environments)
    - [Story 2: a pool that downloads model weights](#story-2-a-pool-that-downloads-model-weights)
    - [Story 3: a mixed fleet](#story-3-a-mixed-fleet)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
      - [Where the gate goes](#where-the-gate-goes)
      - [Counting warming sandboxes](#counting-warming-sandboxes)
      - [What the bound does and does not guarantee](#what-the-bound-does-and-does-not-guarantee)
      - [Resolving maxConcurrent](#resolving-maxconcurrent)
      - [Driving the next create](#driving-the-next-create)
      - [Status](#status)
      - [Interaction with the readiness grace period](#interaction-with-the-readiness-grace-period)
      - [Interaction with the Recreate update strategy](#interaction-with-the-recreate-update-strategy)
      - [Interaction with the existing shaping controls](#interaction-with-the-existing-shaping-controls)
      - [Test plan](#test-plan)
- [Scalability](#scalability)
- [Alternatives](#alternatives)
  - [Tell users to set --sandbox-warm-pool-max-refill-rate instead](#tell-users-to-set---sandbox-warm-pool-max-refill-rate-instead)
  - [Move the refill rate into the SandboxWarmPool spec](#move-the-refill-rate-into-the-sandboxwarmpool-spec)
  - [Rely on pod resource requests and the scheduler](#rely-on-pod-resource-requests-and-the-scheduler)
  - [Add a type enum instead of a single quantity](#add-a-type-enum-instead-of-a-single-quantity)
  - [Count every non-Ready member, including unschedulable ones](#count-every-non-ready-member-including-unschedulable-ones)
<!-- /toc -->

## Summary

A `SandboxWarmPool` creates every missing sandbox at once. When a pool is first
created, or when a burst of `SandboxClaim`s drains it, the controller issues the
whole deficit in a few batches. For sandboxes whose startup does heavy work —
compiling an application, seeding a database, downloading model weights — the
resulting flood of simultaneous startups can overload the cluster.

This KEP adds `spec.warmupStrategy.maxConcurrent` to `SandboxWarmPool`. The
field bounds how many sandboxes the pool warms up at the same time, as an
absolute number or as a percentage of `spec.replicas`. The controller creates a
replacement only after an earlier sandbox finishes warming up, so the bound
holds no matter how long warmup takes. Pools that do not set the field behave
exactly as they do today.

## Motivation

A pool member is "warming up" from the moment it is created until the `Sandbox`
reports `Ready`. Once its Pod is scheduled the member consumes node CPU and
memory for the rest of that window. The window is counted from creation rather
than from scheduling so that a member holds its place as soon as it exists,
which is what keeps the bound in this KEP from being overshot. For a trivial
sandbox the window is a few seconds and nobody notices.
For a sandbox that builds an application on startup the window can be tens of
minutes of near-saturated CPU.

The controller has no notion of this window. `reconcilePool` computes the
deficit between `spec.replicas` and the pool's live population, and creates
toward it immediately. A new 20-replica pool therefore starts 20 heavy builds
within seconds of each other. The same happens whenever claims drain the pool.

Operators hit this as cluster-wide degradation: node pressure, evictions,
scheduling delays for unrelated workloads, and — because contention slows every
build down — warmup windows that stretch far past their normal duration.

### Why the existing shaping controls do not solve this

The controller already shapes pool refill through three controller flags:

| Flag | Default | What it bounds |
| --- | --- | --- |
| `--sandbox-warm-pool-max-batch-size` | `300` | Sandboxes created per reconcile pass |
| `--sandbox-warm-pool-max-refill-rate` | `0` (off) | Sandbox creates per second, per pool |
| `--sandbox-warm-pool-replenish-delay` | `0` (off) | How long refill waits after members drop out |

All three bound **API writes**. None of them bounds **concurrent warmup work**.
The expectations gate that paces batches lowers its count when the informer
observes the `Sandbox` add event, milliseconds after the `POST` — not when the
sandbox becomes `Ready`. So a pool can have every one of its sandboxes warming
up simultaneously while still respecting all three controls.

`--sandbox-warm-pool-max-refill-rate` comes closest, because a create rate does
bound concurrency indirectly. By Little's Law:

```
concurrent warmups  ≈  create rate × warmup duration
```

That relationship is the problem, for three reasons.

**It is open-loop.** The rate cap fires on a clock and never observes the
cluster. Pick a rate assuming a 20-minute warmup, and the bound holds only while
warmup takes 20 minutes. Contention makes warmup slower, which raises
concurrency, which raises contention:

| Actual warmup duration | Concurrent warmups at `rate=0.00083/s` | With `maxConcurrent: 1` |
| --- | --- | --- |
| 20 min (as planned) | 1 | 1 |
| 40 min | 2 | 1 |
| 60 min | 3 | 1 |
| 80 min | 4 | 1 |

The degradation the operator is trying to prevent is what drives the overshoot.
A concurrency bound does not drift this way, because it releases a new create
only when an existing warmup finishes, so a longer warmup delays the next create
instead of stacking another one on top. This is the ordinary reason
resource-bound work uses a semaphore rather than a rate limiter.

The bound is not absolute, and
[What the bound does and does not guarantee](#what-the-bound-does-and-does-not-guarantee)
sets out the two cases where concurrent warmups can still exceed it. Neither is
driven by warmup duration, which is the failure mode this table is about.

**The values are not expressible in practice.** Holding one 20-minute warmup at
a time requires `rate = 1/1200 ≈ 0.00083`. That number is a magic constant
derived from a measurement that drifts, and no rate value means "exactly one at
a time."

**It is a controller flag, so it is cluster-wide.** One controller serves every
`SandboxWarmPool`. An operator running a heavy build pool beside a lightweight
pool must set the flag for the heaviest workload and throttle everything else.
Issue
[#1293](https://github.com/kubernetes-sigs/agent-sandbox/issues/1293) makes the
same argument for the readiness grace period and the unschedulable re-check
interval.

### Goals

- Let a `SandboxWarmPool` bound how many of its sandboxes warm up at the same
  time, independent of how long warmup takes.
- Express the bound as an absolute number or a percentage of `spec.replicas`.
- Apply the bound to every pool create: initial fill, scale-up, replenishment
  after claims adopt members, and replacement of sandboxes removed as stuck.
- Leave the behavior of pools that do not set the field byte-for-byte unchanged.
- Report enough status for an operator to tell a deliberately slow fill from a
  wedged pool.

### Non-Goals

- Replacing or deprecating the three existing shaping flags. They protect the
  API server write path, which this feature does not address.
- Staging the delete side of the `Recreate` update strategy. A `maxUnavailable`
  for template rollouts is a reasonable follow-up and is deliberately out of
  scope.
- Recommending a value. Warmup cost varies by orders of magnitude between
  workloads, so the project should document how to derive a value, not suggest
  one.
- Making the readiness grace period configurable per pool. That is
  [#1293](https://github.com/kubernetes-sigs/agent-sandbox/issues/1293), and
  this KEP depends on it for mixed fleets (see
  [Interaction with the readiness grace period](#interaction-with-the-readiness-grace-period)).

## Proposal

Add an optional `warmupStrategy` to `SandboxWarmPoolSpec` with a single field,
`maxConcurrent`. Before issuing creates, the reconciler counts the pool members
that are currently warming up and creates at most
`max(0, maxConcurrent - warmingUp)` sandboxes. The floor matters: warming
members can exceed `maxConcurrent` when members held while unschedulable are
scheduled together, so the subtraction can go negative.

### User Stories

#### Story 1: a pool of pre-built development environments

As a platform operator, I keep a pool of ready-to-use development sandboxes. On
startup each sandbox compiles a frontend and a backend and seeds a database,
which takes about 20 minutes of heavy CPU and memory. When I create the pool, or
when my team claims several sandboxes at once, every replacement starts building
simultaneously and the cluster degrades for everyone.

I set `maxConcurrent: 1` and accept a slow fill in exchange for a cluster that
stays responsive.

#### Story 2: a pool that downloads model weights

As an ML platform operator, my sandboxes pull several gigabytes of model weights
on startup. Twenty simultaneous pulls saturate my egress and the registry
throttles me, which makes every pull slower.

I set `maxConcurrent: "25%"` so the pool fills in four waves, and the bound
scales automatically when I change `spec.replicas`.

#### Story 3: a mixed fleet

As an operator, I run a heavy build pool and a lightweight pool in one cluster.
A controller flag forces one setting on both. I want the heavy pool serialized
and the lightweight pool to fill at once, so I set `maxConcurrent` on the heavy
pool and leave it unset on the other.

### High-Level Design

The reconciler gains one clamp on the number of sandboxes it may create in a
pass. The clamp is derived from live cluster state — how many members are
currently warming up — rather than from a timer, which is what makes the bound
hold under changing warmup durations.

#### API Changes

A new optional field on `SandboxWarmPoolSpec` in
`extensions/api/v1beta1/sandboxwarmpool_types.go`:

```go
// SandboxWarmPoolSpec defines the desired state of SandboxWarmPool.
type SandboxWarmPoolSpec struct {
	// ... existing fields ...

	// warmupStrategy bounds how many sandboxes the pool warms up at the same
	// time. Leave it unset to create the whole deficit at once.
	// +optional
	WarmupStrategy *SandboxWarmPoolWarmupStrategy `json:"warmupStrategy,omitempty"`
}

// SandboxWarmPoolWarmupStrategy bounds how many sandboxes a pool warms up
// concurrently. A sandbox warms up from the moment it is created until the
// Sandbox reports Ready, and it consumes node CPU and memory from the point its
// Pod is scheduled onward. Pools whose sandboxes do heavy startup work, such as
// compiling an application or downloading model weights, can overload a cluster
// when they fill a large deficit all at once.
type SandboxWarmPoolWarmupStrategy struct {
	// maxConcurrent is the largest number of sandboxes the pool warms up at the
	// same time, as an absolute number (for example 1) or as a percentage of
	// spec.replicas (for example "25%"). Percentages round up and never resolve
	// below 1, so the pool always makes progress.
	//
	// The pool creates a new sandbox only once an earlier one finishes warming
	// up, so this bound holds regardless of how long warmup takes. Sandboxes
	// whose Pods are unschedulable or waiting on scheduling gates do not count
	// against it, because they consume no node resources; they can therefore
	// all be scheduled at once when capacity returns.
	//
	// This bounds the sandboxes the pool itself is warming, not total warmup
	// load on the cluster. A sandbox adopted by a SandboxClaim before it
	// becomes Ready leaves the pool and stops counting while its startup work
	// continues.
	//
	// Leaving this unset creates the whole deficit at once.
	// +optional
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:XValidation:rule="(type(self) == int && self >= 1) || (type(self) == string && self.matches('^([1-9][0-9]?|100)%$'))",message="maxConcurrent must be an integer of at least 1 or a percentage from 1% to 100%"
	MaxConcurrent *intstr.IntOrString `json:"maxConcurrent,omitempty"`
}
```

The field mirrors `Deployment`'s `maxSurge` and `maxUnavailable`.
`+kubebuilder:validation:XIntOrString` is required for controller-tools to emit
the `x-kubernetes-int-or-string` schema, and the CEL rule enforces both accepted
forms: an integer of at least 1, or a percentage string from `"1%"` to `"100%"`.
The exact rule is a starting point for review rather than a settled detail.

A new status field reports the count the gate acts on:

```go
// SandboxWarmPoolStatus defines the observed state of SandboxWarmPool.
type SandboxWarmPoolStatus struct {
	// ... existing fields ...

	// warmingUpReplicas is the number of active pool sandboxes that have not
	// reported Ready, excluding those whose mirrored PodScheduled condition
	// reports Unschedulable or SchedulingGated. These are the sandboxes that
	// count against spec.warmupStrategy.maxConcurrent.
	// +optional
	// +kubebuilder:validation:Minimum=0
	WarmingUpReplicas int32 `json:"warmingUpReplicas,omitempty"`
}
```

Example:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: devbox-pool
spec:
  replicas: 10
  sandboxTemplateRef:
    name: devbox
  warmupStrategy:
    maxConcurrent: 1
```

Both fields are optional and their zero values reproduce current behavior, so
this is an additive change and not a breaking one.

#### Implementation Guidance

All controller changes are in
`extensions/controllers/sandboxwarmpool_controller.go`.

##### Where the gate goes

`reconcilePool` already narrows the deficit through a sequence of clamps before
issuing creates. The new clamp slots into that sequence:

```
deficit = desiredReplicas - totalReplicas
  |
  |-- 1. ReplenishDelay    skip this pass entirely?            (existing)
  |-- 2. MaxBatchSize      clamp to the per-pass cap           (existing)
  |-- 3. warmup slots      clamp to max(0, maxConcurrent - warmingUp)  (NEW)
  |-- 4. MaxRefillRate     token bucket                        (existing)
  |-- 5. expectations      have prior writes been observed?    (existing)
  |-- 6. slowStartBatch    issue the creates                   (existing)
```

Place the clamp before the token bucket. Taking tokens first would spend them on
a pass the warmup gate then withholds, so every gated pass would need a refund,
and the bucket's refund path exists for the expectations gate only.

##### Counting warming sandboxes

The reconciler already walks the pool's active members to classify them by
readiness and scheduling state. Count warming members in that same loop:

> A member is warming up when it is active, is not `Ready`, and
> `podNotScheduledReason` returns neither `Unschedulable` nor
> `SchedulingGated`.

The predicate is stated as an exclusion rather than as `PodScheduled=True`, and
that distinction is load-bearing in both directions.

**Excluding unschedulable and gated members prevents a deadlock.** A Pod in
either state is not running and consumes no node CPU or memory, so it is not the
work the bound exists to limit. The reconciler also holds such members
indefinitely rather than replacing them, so counting them would mean a pool with
`maxConcurrent: 1` whose single member cannot be scheduled never creates
anything again.

**Counting members that have not been scheduled yet prevents an overshoot.** A
just-created member has no mirrored `PodScheduled` condition at all. Under a
strict `PodScheduled=True` test the reconcile fired by that member's own add
event would see `warmingUp = 0` and create another, repeating until the pool is
full and defeating the bound entirely. Treating "not yet scheduled" as occupying
a slot closes that window, because the slot is held from creation onward.

This is exactly what the existing `podNotScheduledReason` helper already
provides, so no new condition parsing is needed. The helper returns `""` for a
`True`, `Unknown`, or absent condition alike, and returns the reason only when
`PodScheduled=False`. Every case the helper collapses into `""` is a case that
should hold a slot, so the ambiguity is harmless here rather than something to
work around. Reuse the helper rather than reading Pods; the condition is already
mirrored onto `Sandbox.status`.

##### What the bound does and does not guarantee

The bound applies to the sandboxes a pool is currently warming. It is not a cap
on total warmup load across the cluster, and the KEP should not be read as
promising one. Two cases make the difference visible, and both should be stated
in the field's godoc.

**A claim can adopt a member mid-warmup.** `getCandidate` prefers a `Ready`
candidate, but when none exists it parks an unready one as a fallback and adopts
it. `completeAdoption` then deletes `warmPoolSandboxLabel` from the sandbox, so
it leaves the pool's label selector and stops counting against `maxConcurrent`
while its startup work continues on the node. The pool then refills, and the
replacement starts warming beside it. With `maxConcurrent: 1` and a steady
stream of claims, node load can therefore exceed one concurrent warmup even
though the pool never has more than one warming member of its own.

Tracking adopted sandboxes through their startup would mean the pool controller
reasoning about objects it no longer owns, which is a significant expansion of
the pool's responsibility. This KEP proposes narrowing the documented guarantee
instead: `maxConcurrent` bounds the pool's own warming members.

**Members excluded while unschedulable can schedule together.** Unschedulable
and gated members hold no slot, so a pool short on capacity keeps creating up to
`maxConcurrent` more. When capacity later frees up, every held member can be
scheduled at once and exceed the bound. This is the direct cost of the deadlock
avoidance above, and the trade is deliberate: a pool that stops creating forever
is worse than a transient overshoot when capacity returns.

Pod resource requests limit the damage but do not close the gap. They bound what
a node admits by resource capacity, so they cap how much warmup work lands on
any one node. They do not enforce `maxConcurrent`, and nothing proposed here
does under this condition: a cluster with capacity for every held member will
schedule them all at once. Operators who cannot tolerate that should keep the
pool smaller than the burst they are willing to absorb.

##### Resolving maxConcurrent

Resolve the value against `desiredReplicas` with
`intstr.GetScaledValueFromIntOrPercent(value, int(desiredReplicas), true)`,
rounding up. Clamp the result to a floor of 1 so that a small percentage of a
small pool, such as `"10%"` of 5 replicas, never resolves to 0 and wedges the
pool. When `WarmupStrategy` or `MaxConcurrent` is nil, skip the clamp entirely
so the unset path keeps its current behavior and cost.

##### Driving the next create

No polling is needed. The controller already watches `Sandbox` objects through
`Owns`-style ownership handling, so a member flipping to `Ready` enqueues its
pool and the next create follows within milliseconds. Add a conservative
fallback requeue for the case where the gate withholds every create, so a lost
watch event cannot idle a pool until the next resync.

##### Status

Set `status.warmingUpReplicas` from the same count the gate uses, and log at
`V(4)` when the gate withholds creates, including the resolved `maxConcurrent`
and the current `warmingUp`. A `maxConcurrent: 1` pool with a 20-minute warmup
takes hours to fill, and an operator needs to distinguish that from a wedged
pool without reading controller logs.

The existing `agent_sandbox_warmpool_size` metric already separates ready from
pending members, so no new metric is proposed.

##### Interaction with the readiness grace period

The controller deletes and replaces a pool member that has not reported `Ready`
within `--sandbox-warm-pool-readiness-grace-period`, which defaults to 5
minutes. Any pool with a warmup longer than that is already churning today,
before this feature exists: every member is destroyed and recreated before it
can finish, and the pool never reaches `Ready`.

This feature therefore only helps operators who also raise the grace period
above their warmup duration. That is currently a controller flag, so a mixed
fleet cannot set it correctly for every pool — exactly the gap
[#1293](https://github.com/kubernetes-sigs/agent-sandbox/issues/1293) describes.
That issue is marked `priority/awaiting-more-evidence`; a pool with a 20-minute
build-and-seed warmup is concrete evidence for it.

The two features compose cleanly and can land independently. This KEP proposes
documenting the dependency prominently in the field's godoc and in
`docs/configuration.md`, and adopting whatever precedence rule #1293 settles on
if a per-pool grace period lands.

One consequence to accept explicitly: a genuinely stuck member occupies a
warmup slot until the grace period expires. With `maxConcurrent: 1` the pool
makes no progress for that long. This is visible in
`status.warmingUpReplicas` and is the correct trade, since the alternative is
replacing a member that might simply be slow.

##### Interaction with the Recreate update strategy

A pool using the `Recreate` update strategy deletes all of its stale unclaimed
members as soon as the template changes. Refill is then gated, so the pool sits
near-empty for roughly `(replicas / maxConcurrent) × warmup duration` while it
rebuilds. With `maxConcurrent: 1` and a long warmup that is a multi-hour window
in which claims fall through to cold start.

The gate does not cause this — `Recreate` empties the pool today just as
quickly — but it extends how long the pool stays empty afterward, so operators
must see it stated before choosing both settings together. `OnReplenish`, the
default, does not have this problem, because it replaces stale members only as
they are claimed or deleted.

This is the concrete reason the `maxUnavailable` follow-up listed in
[Non-Goals](#non-goals) matters: staging the delete side would let a `Recreate`
rollout keep serving claims while it rolls. Until that exists, the KEP proposes
documenting `Recreate` plus a low `maxConcurrent` as a combination to adopt
deliberately.

##### Interaction with the existing shaping controls

The clamps compose and the tightest one wins. Guidance to document, as a
decision rule rather than a recommended configuration:

| Bottleneck | Control |
| --- | --- |
| API server write path, etcd, APF throttling | `--sandbox-warm-pool-max-refill-rate` |
| Node CPU and memory during warmup | `spec.warmupStrategy.maxConcurrent` |
| Both | Both |

`maxConcurrent` does nothing for write pressure: `maxConcurrent: 500` still
issues a 500-create burst. For short and uniform warmups the rate flag remains
the better tool.

##### Test plan

Unit tests in `extensions/controllers/sandboxwarmpool_controller_test.go`,
following the existing fake-client and fake-clock harness used by
`TestReconcilePoolMaxRefillRate`:

- `maxConcurrent: 1` against a 10-sandbox deficit creates exactly one, and
  creates the second only after the first reports `Ready`.
- `maxConcurrent: "50%"` of 10 replicas creates 5, then 5 more.
- Percentages round up and floor at 1: `"10%"` of 5 replicas resolves to 1.
- A member with no mirrored `PodScheduled` condition occupies a slot. This is
  the regression test for the overshoot described above: the reconcile fired by
  a new member's own add event must not create a second one.
- A member whose `PodScheduled` condition is `Unknown` occupies a slot.
- An unschedulable member does not occupy a slot. This is the regression test
  for the deadlock described above.
- A member adopted by a claim while still warming leaves the count and frees its
  slot, matching the narrowed guarantee rather than contradicting it.
- An unset `warmupStrategy` reproduces current at-once behavior.
- The gate composes correctly with `MaxRefillRate` and `MaxBatchSize`.
- `status.warmingUpReplicas` matches the members the gate counts.
- A member removed by the grace-period GC frees its slot.

An e2e test is not proposed. The behavior is a controller-internal scheduling
decision that unit tests cover deterministically, and an honest e2e test would
have to wait out real warmup windows. This follows the direction of
[#1200](https://github.com/kubernetes-sigs/agent-sandbox/issues/1200).

## Scalability

The gate reduces load rather than adding it, and its cost is negligible.

**Reconcile cost.** Counting warming members adds one condition lookup per
active member inside a loop that already walks every active member and already
reads the same two conditions. For pools that do not set the field the clamp is
a nil check. No additional API calls or cache reads.

**Reconcile frequency.** The gate does not add polling. Progress is driven by
`Sandbox` watch events the controller already receives. A gated pool reconciles
once per completed warmup plus the fallback requeue, which is far less often
than an ungated pool filling the same deficit.

**API server load.** No higher, and lower whenever the gate is active. A pool
whose `maxConcurrent` is unset, or is at least as large as the current deficit,
withholds nothing and issues exactly the writes it does today. The gate
withholds creates, so a pool using
it issues the same total number of creates spread over a longer period. It never
increases burst size.

**Cluster load.** This is the point of the feature. Peak concurrent warmup CPU
and memory drops from `replicas × per-sandbox cost` to
`maxConcurrent × per-sandbox cost`.

**Time to fill.** The cost the operator accepts, and the reason the project
should not recommend a value:

```
fill time  ~=  (replicas / maxConcurrent) x warmup duration
```

A 100-replica pool with a 20-minute warmup fills in roughly 40 minutes at
`maxConcurrent: "50%"`, 3.3 hours at `maxConcurrent: 10`, and 33 hours at
`maxConcurrent: 1`. Documentation should give operators this formula and the
corresponding load formula, `maxConcurrent × per-sandbox warmup cost`, so they
can choose a point on that curve for their own workload.

**Many pools.** Per-pool state is unchanged; the gate derives its count from
objects already in the informer cache and stores nothing new. Pools remain
independent and reconcile in parallel up to
`--sandbox-warm-pool-concurrent-workers`.

## Alternatives

### Tell users to set --sandbox-warm-pool-max-refill-rate instead

Rejected as a complete answer, and explicitly retained as a complementary
control. A create rate bounds concurrency only through
`concurrency ≈ rate × warmup duration`, which requires the operator to know the
warmup duration and requires it to stay stable. It is open-loop, so it overshoots
precisely when contention slows warmup down, as the table in
[Motivation](#why-the-existing-shaping-controls-do-not-solve-this) shows. It is
also cluster-wide, and the values needed for long warmups are unintuitive magic
constants.

The two controls protect different resources and should both exist.

### Move the refill rate into the SandboxWarmPool spec

Rejected. A per-pool rate fixes the cluster-wide problem but leaves the
open-loop behavior, the overshoot under feedback, and the magic constants
untouched. It would also add a second per-pool pacing knob overlapping this one,
which `AGENTS.md` warns against.

### Rely on pod resource requests and the scheduler

Rejected as a substitute, recommended as a complement. Accurate
`resources.requests` do stop a node from being over-admitted, and the controller
already holds unschedulable members rather than churning them. But the sandboxes
are still created: they pile up as `Pending` Pods, and a cluster autoscaler will
add nodes to fit them. That bounds per-node contention while increasing cost,
whereas this feature avoids creating the work at all.

### Add a type enum instead of a single quantity

Considered: a `warmupStrategy.type` enum of `AtOnce`, `OneByOne`, and
`Percentage`, with a separate value field.

Rejected. `maxConcurrent: 1` already expresses one-by-one and `"50%"` expresses
a percentage, so the enum would add a second way to say the same thing and an
invalid-combination surface to validate. `AGENTS.md` warns against overlapping
configuration mechanisms, and `Deployment.spec.strategy.rollingUpdate` sets the
precedent for an `IntOrString` quantity without a companion enum. The nested
struct keeps room to add fields later without a breaking change.

### Count every non-Ready member, including unschedulable ones

Rejected. It deadlocks. The reconciler holds unschedulable and scheduling-gated
members indefinitely rather than replacing them, so under a capacity shortfall a
pool with `maxConcurrent: 1` would never create another sandbox. Counting only
scheduled members also matches the intent more closely, since a Pod that is not
running consumes no node CPU or memory.
