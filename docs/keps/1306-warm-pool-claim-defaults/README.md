# KEP-1306: Warm Pool Claim Defaults

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [The production viability question](#the-production-viability-question)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API Changes](#api-changes)
  - [Controller Implementation](#controller-implementation)
  - [Version Conversion (v1alpha1)](#version-conversion-v1alpha1)
  - [Test Plan](#test-plan)
  - [Upgrade / Downgrade Strategy](#upgrade--downgrade-strategy)
- [Alternatives Considered](#alternatives-considered)
  - [1. Change the Global Default to Delete - <em>Rejected</em>](#1-change-the-global-default-to-delete---rejected)
  - [2. Controller-Side Injection on Every Reconcile - <em>Rejected</em>](#2-controller-side-injection-on-every-reconcile---rejected)
  - [3. Mutating Admission Webhook - <em>Deferred</em>](#3-mutating-admission-webhook---deferred)
  - [4. Place claimDefaults on SandboxTemplate - <em>Rejected</em>](#4-place-claimdefaults-on-sandboxtemplate---rejected)
- [Scalability](#scalability)
<!-- /toc -->

## Summary

Add an optional `claimDefaults` field to `SandboxWarmPoolSpec` that lets pool
operators declare default lifecycle settings for claims. When a `SandboxClaim`
with `Lifecycle: nil` targets a pool that has `claimDefaults.lifecycle`
configured, the controller copies the pool's lifecycle into the claim — both
on warm adoption and cold fallback (pool exhaustion). Claims that set their
own `Lifecycle` are never modified.

## Motivation

`SandboxClaim.Spec.Lifecycle` is an optional pointer field. When omitted
(nil), the claim has **no lifecycle management at all** — no expiration, no
TTL, no automatic cleanup. The controller's `checkExpiration` returns early
for nil Lifecycle (`sandboxclaim_controller.go:477-479`). If a `Lifecycle` is
set, its `ShutdownPolicy` defaults to `Retain` (via `+kubebuilder:default`),
which preserves the claim object after expiration for auditing. Both behaviors
— nil Lifecycle and explicit Retain — are safe for interactive notebooks and
developer environments where claims are manually managed.

However, warm pool claims are ephemeral by definition. An agent creates a
claim, executes code, and discards it. Without a lifecycle:

- **Zombie VMs accumulate.** A completed Kata claim holds real hypervisor
  processes, CPU, and memory until explicitly deleted. If the SDK client
  crashes without calling `Close()`, the claim and its VM live forever.
- **Pool starvation under sustained load.** On a 30-replica kata-qemu pool
  with 3-minute sustained burst, nil-lifecycle claims cause ready replicas to
  drop from 27/30 to 0/30 after ~90 claims. All 30 vCPUs are consumed by
  uncollected VMs that will never be reclaimed ([benchmark data][1306]).
- **runc masks the problem.** A completed runc container releases cgroups
  immediately, so the nil-lifecycle default causes object accumulation but not
  resource exhaustion. Kata VMs hold real hypervisor processes and memory;
  GPU-attached pods hold device allocations. These resources are not
  reclaimed until the pod is explicitly deleted.
- **Suspend-resume does not help for VMs today.** For runc and gVisor,
  agent-sandbox suspend/resume ([KEP-694][694], `operatingMode: Suspended`)
  is a cheap pod delete/create cycle (~1-3s). An idle agent can suspend its
  sandbox and resume the same claim later, reducing total claim churn. For
  Kata, there is no practical resource-efficient suspend in the current
  Kubernetes integration:

  - **Container-level pause** exists ([kata-containers#6434][kata-6434],
    [kata-containers#8023][kata-8023]): the kata-agent freezes the process
    inside the VM, but the VM itself stays running — QEMU process, guest
    kernel, agent, and all memory remain allocated. This was a deliberate
    design choice ([kata-containers/runtime#328][kata-328]): pausing the VM
    would break agent communication.
  - **VM-level pause** (QMP `stop`) freezes vCPUs but holds all resources.
    Not exposed to Kubernetes.
  - **CRIU checkpoint** is not supported (guest kernel lacks
    `CONFIG_CHECKPOINT_RESTORE`).

  The underlying hypervisors *do* support VM state snapshots — Firecracker
  has production-grade snapshot/restore (used by AWS Lambda for ~4-20ms
  cold starts via memory demand-paging with userfaultfd) and Cloud
  Hypervisor implements similar VMM state serialization. However, even
  when hypervisor-level snapshots work, applying them to general-purpose
  workloads introduces problems that AWS had to solve with **new Linux
  kernel interfaces** ([Agache et al., 2021][firecracker-paper]):

  - **Uniqueness loss**: cloned VMs generate duplicate UUIDs, cryptographic
    keys, and PRNG state. Requires `MADV_WIPEONSUSPEND` (zeroes memory
    pages on suspend) and `SysGenId` (generation counter) in the guest
    kernel — neither is mainlined in standard distro kernels. Even with
    `VmGenID`, only the kernel's entropy pool is re-seeded — user-space
    PRNGs already initialized in memory (e.g., `numpy.random`, OpenSSL
    contexts cached in Python) retain stale state and will generate
    duplicate nonces across restored clones.
  - **Secret leakage**: VM memory is serialized to disk, including
    cryptographic keys and tokens. For sandboxes running untrusted code,
    this creates an attack vector the threat model doesn't currently cover.
  - **Network and storage breakage**: TCP and TLS sessions cannot survive
    snapshot/restore — active HTTP connections between the sandbox and the
    SDK router would be invalidated. Beyond TCP, CNI network sockets are
    dropped, Pod IP re-binding after restore is not guaranteed, virtiofs
    shared storage may desync, and guest clock drift immediately breaks
    kubelet projected service account tokens (JWT `exp` validation) and
    TLS handshakes that depend on wall-clock time.
  - **Controlled platform vs. arbitrary workloads**: Lambda can enforce
    snapshot safety because AWS controls the entire guest kernel, driver
    stack, and managed runtime. agent-sandbox must run un-patched,
    arbitrary user container images that know nothing about `VmGenId` or
    custom memory flags. Lambda explicitly tracks in-flight requests and
    ensures VMs cannot act outside request context — this orchestration
    does not exist in the CRI → kubelet → kata shim path.

  **Kata-containers only uses snapshot capabilities for VM templating** —
  snapshotting the pre-boot state (kernel + agent + initramfs) to clone
  fresh VMs faster, not to capture or restore running workload state
  ([kata VM templating docs][kata-templating]). The path from Kubernetes
  pod API through CRI, the OCI runtime spec, and the kata shim has no
  snapshot/restore verb for running workloads. Bridging this gap would
  require extensions at every layer — CRI, OCI, kata shim, and
  agent-sandbox controller — plus solving the uniqueness, secret leakage,
  and network session problems at the guest kernel level. This is a
  multi-year effort orthogonal to this KEP.

  Agent-sandbox suspend therefore **destroys the entire VM** and boots a
  fresh one on resume (5-15s cold start). There is no middle ground between
  "hold the full VM" and "destroy it completely."

  This means a kata-backed agent that goes idle must either hold the VM
  (wasting ~256MB+ memory per sandbox) or release it and claim a new one
  on wake-up. Both paths produce higher claim churn than runc/gVisor,
  making auto-cleanup via `claimDefaults` even more critical for
  VM-backed pools.

- **Security gap.** A retained sandbox with a completed-but-not-deleted pod
  retains network access, mounted secrets, and service account tokens (if
  enabled). The [threat model](../../security/threat_model.md) identifies
  untrusted LLM-generated code as the primary threat vector. Zombie
  sandboxes are unnecessary attack surface that `Delete + TTL` eliminates.

### The production viability question

Without a mechanism for pool operators to enforce claim cleanup, **VM-backed
warm pools are not viable in production.** The issue is not theoretical:

- Benchmark data on a 30-replica kata-qemu pool shows pool starvation after
  ~90 claims under sustained load with nil-lifecycle claims ([#1306][1306]).
- Every SDK crash, network partition, or agent that omits `Close()` leaves a
  VM running indefinitely — consuming real CPU, memory, and hypervisor
  processes. `claimDefaults` with `Delete + TTL` handles the completed-
  workload case; crash-with-running-pod is a liveness/session concern handled
  by the SDK router's heartbeat mechanism.
- The "document it" mitigation pushes the burden to every SDK integrator.
  In a multi-tenant platform with third-party agent frameworks, some will
  not comply. One non-compliant integration is enough to starve a shared
  pool.
- Suspend-resume cannot reduce claim churn for VMs (see above). runc and
  gVisor can partially paper over the issue because suspend is cheap and
  completed containers release resources immediately. VMs cannot.

If this gap is not closed, agent-sandbox's production scope is effectively
limited to userspace runtimes (runc, gVisor) — which defeats the kernel-level
isolation promise that motivates Kata integration in the first place. The
project positions itself as the secure execution layer for multi-tenant
agentic AI on platforms like OpenShift with Kata Containers. That positioning
requires VM-backed pools that self-heal under real-world failure patterns,
not pools that collapse when an SDK client crashes.

`claimDefaults` is the minimum viable fix: it lets pool operators express
cleanup policy once, at the pool level, without changing any global defaults
or requiring every SDK integration to independently discover and implement
the same workaround.

[694]: ../694-kep-for-suspend-and-resume-for-beta/README.md
[kata-6434]: https://github.com/kata-containers/kata-containers/issues/6434
[kata-8023]: https://github.com/kata-containers/kata-containers/pull/8023
[kata-328]: https://github.com/kata-containers/runtime/pull/328
[kata-templating]: https://github.com/kata-containers/kata-containers/blob/main/docs/how-to/what-is-vm-templating-and-how-do-I-use-it.md
[firecracker-paper]: https://arxiv.org/abs/2102.12892
[1306]: https://github.com/kubernetes-sigs/agent-sandbox/issues/1306

### Goals

1. Let pool operators declare default `Lifecycle` settings for claims targeting
   their pool, without changing the global `SandboxClaim` default.
2. Apply defaults at claim creation time (warm adoption or cold fallback),
   never retroactively.
3. Preserve explicit claim-level `Lifecycle` settings — no silent override.
4. Ensure the field survives v1alpha1 ↔ v1beta1 conversion round-trips.

### Non-Goals

- Changing the global default `ShutdownPolicy` from `Retain` to `Delete`.
- Adding a mutating admission webhook (may be considered in the future).
- Providing defaults for fields other than `Lifecycle` (e.g., resource limits,
  network policy). The `ClaimDefaults` struct is extensible, but only
  `Lifecycle` is proposed in this KEP.

## Proposal

### User Stories

**Story 1: Platform administrator managing a Kata warm pool**

As a platform admin, I create warm pools backed by Kata VMs for multi-tenant
agent workloads. I want claims from these pools to auto-delete after the
workload finishes, so that VMs are reclaimed and the pool can refill. I should
not have to modify every SDK client or agent framework that creates claims.

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: kata-agent-pool
spec:
  replicas: 20
  sandboxTemplateRef:
    name: kata-sandbox
  claimDefaults:
    lifecycle:
      shutdownPolicy: Delete
      ttlSecondsAfterFinished: 10
```

Any `SandboxClaim` targeting `kata-agent-pool` with `Lifecycle: nil` will
inherit `Delete + TTL=10s`. A claim that explicitly sets `Retain` keeps its
setting.

**Story 2: Developer using the default pool for debugging**

As a developer, I use a warm pool with no `claimDefaults` configured. My
claims default to `Retain` as today, so I can inspect the sandbox state after
my code finishes. Nothing changes for me.

**Story 3: SDK integration with explicit lifecycle**

As an SDK author, I set `ShutdownPolicy: Delete` and `TTL: 30` on every claim
my framework creates. When the pool also has `claimDefaults.lifecycle`, my
explicit setting takes precedence. The pool's defaults are a safety net for
clients that forget, not an override for clients that choose.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Operator adds `claimDefaults` to a pool with existing `Lifecycle: nil` claims whose workloads have finished | Defaults are applied **only at claim creation** (warm adoption or cold fallback). Existing claims are already bound and never re-enter the creation path. No retroactive deletion. |
| `claimDefaults` lost during v1alpha1 round-trip | State-preservation annotation on the v1alpha1 object stores the serialized `ClaimDefaults`, restored on conversion back to v1beta1. Covered by round-trip test. |
| Confusion about which lifecycle applies | The claim's `Spec.Lifecycle` is always the source of truth. `claimDefaults` is copied into it at creation — after adoption, `kubectl get sandboxclaim -o yaml` shows the effective lifecycle directly. No indirection. |
| Transient `Finished` during suspension could start TTL | When a sandbox is suspended ([KEP-694][694]), the controller deletes the pod. During pod termination, containers that exit non-zero briefly produce a `PodFailed` phase, which `computeFinishedCondition` reflects as `Finished: True` on the sandbox. This condition is **transient** — once the pod is fully removed, the sandbox controller drops `Finished` from the conditions array, and the claim controller mirrors the removal. With `TTL=0`, the claim could theoretically be expired and deleted during the 1-2 second race window before the transient condition is cleaned up. `TTL=10` eliminates this: the 10-second grace period far exceeds the pod termination window, so the transient `Finished` is always cleaned up before expiry fires. The 10-second delay has zero practical impact on resource leak prevention — the problem being solved is VMs running for hours/days, not seconds. |

## Design Details

### API Changes

Add `ClaimDefaults` to `SandboxWarmPoolSpec` in
`extensions/api/v1beta1/sandboxwarmpool_types.go`:

```go
type SandboxWarmPoolSpec struct {
    // ...existing fields (Replicas, TemplateRef, UpdateStrategy)...

    // claimDefaults specifies default values applied to SandboxClaims that
    // target this pool and do not set the corresponding fields themselves.
    // Defaults are applied at claim creation time (warm adoption or cold
    // fallback) and are not retroactively applied to existing claims.
    // +optional
    ClaimDefaults *ClaimDefaults `json:"claimDefaults,omitempty"`
}

// ClaimDefaults defines default values for SandboxClaims targeting a pool.
type ClaimDefaults struct {
    // lifecycle specifies the default lifecycle for claims with nil Lifecycle.
    // If the claim sets its own Lifecycle, this field is ignored.
    // +optional
    Lifecycle *Lifecycle `json:"lifecycle,omitempty"`
}
```

The `Lifecycle` type is already defined in
`extensions/api/v1beta1/sandboxclaim_types.go` and reused here. It carries
the same kubebuilder validation markers (`ShutdownPolicy` enum,
`TTLSecondsAfterFinished` minimum).

**`ShutdownTime` validation:** `Lifecycle` also includes a `ShutdownTime`
field (absolute timestamp). An absolute deadline in `claimDefaults` would be
shared by all adopted claims — claims adopted after that time would expire
immediately. To prevent this, `ClaimDefaults` includes a CEL validation rule
that rejects `ShutdownTime`:

```go
// ClaimDefaults defines default values for SandboxClaims targeting a pool.
// +kubebuilder:validation:XValidation:rule="!has(self.lifecycle) || !has(self.lifecycle.shutdownTime)",message="shutdownTime is not allowed in claimDefaults; use ttlSecondsAfterFinished instead"
// +kubebuilder:validation:XValidation:rule="!has(self.lifecycle) || !has(self.lifecycle.ttlSecondsAfterFinished) || self.lifecycle.ttlSecondsAfterFinished >= 1",message="ttlSecondsAfterFinished in claimDefaults must be at least 1 to avoid premature expiry during transient Finished states"
type ClaimDefaults struct {
    // lifecycle specifies the default lifecycle for claims with nil Lifecycle.
    // If the claim sets its own Lifecycle, this field is ignored.
    // +optional
    Lifecycle *Lifecycle `json:"lifecycle,omitempty"`
}
```

Only `ShutdownPolicy` and `TTLSecondsAfterFinished` are meaningful as pool-
level defaults.

### Controller Implementation

The injection point is `adoptSandboxFromCandidates` in
`extensions/controllers/sandboxclaim_controller.go`, immediately before the
`r.Update(ctx, claim)` call that records the `AssignedSandboxName` annotation
(line ~1143).

Today `adoptSandboxFromCandidates` does not receive the pool object — the pool
is resolved lazily inside `getCandidate` / `resolveRecreate` and is not
returned to the caller. This implementation adds the pool as an explicit
parameter to `adoptSandboxFromCandidates`. The pool is already resolved by
`reconcileSandboxClaim` (via `resolvePool`) before candidates are searched; if
`resolvePool` fails, the error is returned and the claim is requeued — the
adoption path is never reached. This means the pool object is guaranteed to be
non-nil when `adoptSandboxFromCandidates` is called, with no second lookup and
no risk of a cache miss silently skipping defaults:

```go
// Extract pool's claimDefaults. The pool object was already resolved
// by the caller to find warm candidates — no re-fetch needed.
poolLifecycle := resolvePoolLifecycle(pool, claim)

// Apply lifecycle default before the adoption annotation write.
if poolLifecycle != nil {
    claim.Spec.Lifecycle = poolLifecycle.DeepCopy()
}
claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] = adopted.Name
if err := r.Update(ctx, claim); err != nil {
    if !k8errors.IsConflict(err) {
        // ...existing non-conflict error handling...
    }
    // 409 conflict: retry on a fresh base, re-applying both the
    // annotation and the lifecycle default.
    if retryErr := r.retryAdoptionWithDefaults(ctx, claim, adopted.Name, poolLifecycle); retryErr != nil {
        // ...existing retry-failure handling...
    }
}
```

```go
// resolvePoolLifecycle returns the pool's default lifecycle if the claim
// has no lifecycle of its own. Returns nil if either side is unset.
func resolvePoolLifecycle(pool *extensionsv1beta1.SandboxWarmPool, claim *extensionsv1beta1.SandboxClaim) *extensionsv1beta1.Lifecycle {
    if claim.Spec.Lifecycle != nil {
        return nil
    }
    if pool.Spec.ClaimDefaults != nil && pool.Spec.ClaimDefaults.Lifecycle != nil {
        return pool.Spec.ClaimDefaults.Lifecycle.DeepCopy()
    }
    return nil
}
```

The retry function extends `retryAdoptionAnnotation` to also inject the
lifecycle on the fresh object:

```go
func (r *SandboxClaimReconciler) retryAdoptionWithDefaults(
    ctx context.Context,
    claim *extensionsv1beta1.SandboxClaim,
    sandboxName string,
    poolLifecycle *extensionsv1beta1.Lifecycle,
) error {
    return r.updateClaimOnFreshBase(ctx, claim, func(fresh *extensionsv1beta1.SandboxClaim) (bool, error) {
        // ...existing UID and assignment checks from retryAdoptionAnnotation...
        if fresh.Annotations == nil {
            fresh.Annotations = make(map[string]string)
        }
        fresh.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] = sandboxName
        if fresh.Spec.Lifecycle == nil && poolLifecycle != nil {
            fresh.Spec.Lifecycle = poolLifecycle.DeepCopy()
        }
        return true, nil
    })
}
```

**Why this is create-time only:**

1. Lifecycle injection happens once per claim — on the first write that binds
   the claim to a sandbox (warm adoption or cold creation). The nil check
   (`claim.Spec.Lifecycle == nil`) prevents re-injection: once a lifecycle is
   persisted, it survives annotation removal and re-adoption because only
   annotations and labels are cleared, not spec fields.
2. The `r.Update` at this point is a full-object PUT. The lifecycle is persisted
   to the API server atomically with the adoption annotation (warm path) or
   sandbox creation (cold path).
3. On conflict retry (`retryAdoptionWithDefaults`), the claim is re-read fresh
   from the API server. The retry callback re-applies both the annotation and
   the lifecycle default (if the fresh object still has `Lifecycle: nil`). The
   `poolLifecycle` is resolved once before the first attempt and reused across
   retries — no repeated pool lookups.
4. No other code path in the controller writes to `claim.Spec.Lifecycle`. All
   2 `r.Update` and 8 `r.Patch` call sites on claims modify only annotations,
   labels, or the status subresource.

**Pool object reuse:**

The pool object is already resolved in both paths — to find warm candidates
(adoption) or to look up the template (cold creation). `resolvePoolLifecycle`
receives the already-fetched pool; no additional cache read is needed.

**Cold fallback:**

Claims may cold-start for several reasons: pool exhaustion under sustained
load, claims requiring environment or volume injection incompatible with
pre-warmed sandboxes, or pools with zero replicas. Pool exhaustion is the
scenario from the Motivation section where nil-lifecycle claims cause pool
starvation.
Defaults must apply here too — otherwise the leak persists when it matters
most. The `createSandbox` path already resolves the pool via `WarmPoolRef`;
`resolvePoolLifecycle` is called before the sandbox creation write, and the
lifecycle is persisted to the claim atomically via `r.Update`:

```go
// Cold fallback: apply pool defaults before creating a new sandbox.
poolLifecycle := resolvePoolLifecycle(pool, claim)
if poolLifecycle != nil {
    claim.Spec.Lifecycle = poolLifecycle.DeepCopy()
}
// ...proceed with sandbox creation and claim update...
```

Defaults key off `WarmPoolRef` (the claim's declared pool affinity), not the
adoption path. Whether a claim gets a pre-warmed sandbox or a cold-started
one is an implementation detail — the pool operator's cleanup policy applies
either way.

### Version Conversion (v1alpha1)

v1beta1 is the storage version. v1alpha1 has no `ClaimDefaults` field.

The existing conversion already preserves v1alpha1-only state: `ConvertTo`
(v1alpha1 → v1beta1) serializes the full v1alpha1 object into a
`v1alpha1SandboxWarmPoolStateAnnotation` on the v1beta1 object, and
`ConvertFrom` (v1beta1 → v1alpha1) strips it. This handles the v1alpha1 →
v1beta1 → v1alpha1 direction.

For the reverse direction (v1beta1 → v1alpha1 → v1beta1), `ClaimDefaults`
must survive the round-trip. The spec-level helpers (`convertWarmPoolSpecFrom`
/ `convertWarmPoolSpecTo`) only receive `*SandboxWarmPoolSpec` and cannot
access annotations, so the preservation must happen at the **object level**
in `ConvertFrom` and `ConvertTo`:

**v1beta1 → v1alpha1 (`ConvertFrom`):** After `convertWarmPoolSpecFrom`,
serialize `src.Spec.ClaimDefaults` (if non-nil) into a new annotation on the
v1alpha1 object (e.g., `api.agents.x-k8s.io/v1beta1-sandboxwarmpool-state`).
This mirrors the existing pattern but in the reverse direction.

**v1alpha1 → v1beta1 (`ConvertTo`):** After `convertWarmPoolSpecTo`, check
the v1alpha1 object for the v1beta1 state annotation. If present, deserialize
and restore `ClaimDefaults` on the v1beta1 `Spec`. Strip the annotation from
the v1beta1 object so it does not leak.

> **Note on annotation nesting:** `ConvertTo` also serializes the full v1alpha1
> object into `v1alpha1SandboxWarmPoolStateAnnotation`. Because `ConvertFrom`
> writes the v1beta1-state annotation onto the v1alpha1 object, that annotation
> would otherwise be nested inside the serialized blob and resurface on the next
> round-trip. The implementation must strip the v1beta1-state annotation from
> the v1alpha1 object **before** serializing it into
> `v1alpha1SandboxWarmPoolStateAnnotation`.

This ensures lossless round-tripping in both directions: a v1beta1 pool with
`claimDefaults` read via v1alpha1 and written back retains the field, and a
v1beta1-originated pool updated through the v1alpha1 API preserves
`ClaimDefaults`.

### Test Plan

**Unit tests** (`extensions/controllers/sandboxclaim_controller_test.go`):

| Test case | Setup | Expected |
|-----------|-------|----------|
| `warm + nil lifecycle + claimDefaults` | Pool has `Delete+TTL=10` defaults, claim has `Lifecycle: nil` | Claim adopted with `Delete+TTL=10` lifecycle. `kubectl get claim` shows lifecycle. |
| `warm + explicit Retain + claimDefaults` | Pool has `Delete+TTL=10` defaults, claim has explicit `Retain` | Claim lifecycle unchanged. Pool defaults ignored. |
| `warm + nil lifecycle + no claimDefaults` | Pool has no `claimDefaults`, claim has `Lifecycle: nil` | Claim adopted with `Lifecycle: nil`. Behavior identical to today. |
| `warm + nil lifecycle + pool not found` | Pool deleted before adoption completes | Existing `ErrWarmPoolNotFound` handling. No crash. |
| `cold fallback + nil lifecycle + claimDefaults` | No ready candidate in pool, claim falls through to `createSandbox`. Pool has `Delete+TTL=10` defaults. | Claim created with `Delete+TTL=10` lifecycle. Same defaults as warm path. |
| `cold fallback + explicit Retain + claimDefaults` | No ready candidate, pool has `Delete+TTL=10` defaults, claim has explicit `Retain` | Claim lifecycle unchanged. Pool defaults ignored. |
| `cold fallback + nil lifecycle + no claimDefaults` | No ready candidate, pool has no `claimDefaults` | Claim created with `Lifecycle: nil`. Behavior identical to today. |

**Conversion tests** (`extensions/api/v1alpha1/sandboxwarmpool_conversion_test.go`):

| Test case | Expected |
|-----------|----------|
| Round-trip with `ClaimDefaults` populated | v1beta1 → v1alpha1 → v1beta1 preserves `ClaimDefaults` via state annotation |
| Round-trip with `ClaimDefaults` nil | No v1beta1 state annotation on the v1alpha1 object. Restored v1beta1 has nil `ClaimDefaults`. Identical to current behavior. |

**E2E tests** (if appropriate for scope):

- Create pool with `claimDefaults.lifecycle: Delete+TTL=10`, create claim with
  nil lifecycle, verify claim auto-deletes after workload finishes.

### Upgrade / Downgrade Strategy

**Upgrade (old controller → new controller):**

- Existing pools have no `claimDefaults` field (nil). No behavioral change.
- Existing claims already have `AssignedSandboxNameAnnotation` set, so they
  never re-enter the adoption path. No retroactive lifecycle injection.
- The CRD change is purely additive (new optional field). No migration needed.

**Downgrade (new controller → old controller):**

- Pools with `claimDefaults` set are ignored by the old controller (unknown
  field in spec is preserved by the API server but not read by the controller).
- Claims that were adopted with injected lifecycle retain their `Spec.Lifecycle`
  in the API server. The old controller reads `Spec.Lifecycle` normally and
  honors it. No behavioral regression.

**Version skew (mixed controller versions):**

- If multiple controller replicas run different versions during a rolling
  update, a claim may be adopted by either version. The old version ignores
  `claimDefaults` and adopts with nil lifecycle (no expiration, no automatic
  cleanup). The new version applies defaults. This is acceptable: the window
  is brief, and the worst case is a claim with nil lifecycle — the same
  behavior as before this KEP — not a regression.

## Alternatives Considered

### 1. Change the Global Default to Delete - *Rejected*

Change the default `ShutdownPolicy` from `Retain` to `Delete` for all
`SandboxClaim` objects.

**Why rejected:**
- Breaking change for new claims. The CRD only defaults `shutdownPolicy` when
  `lifecycle` is explicitly set — existing claims with `Lifecycle: nil` are
  unaffected by CRD-level defaulting. However, any new claim or update that
  sets `lifecycle` without specifying `shutdownPolicy` would receive `Delete`
  instead of `Retain`, changing cleanup behavior for interactive notebooks
  and developer environments.
- Violates the principle that defaults should err on the side of safety.
  `Retain` is correct for developer environments and interactive notebooks.
- The maintainers explicitly rejected this approach
  ([comment][maintainer-comment]).

[maintainer-comment]: https://github.com/kubernetes-sigs/agent-sandbox/issues/1306#issuecomment-5333749509

### 2. Controller-Side Injection on Every Reconcile - *Rejected*

Inject lifecycle defaults during `checkExpiration` on every reconcile, not
just at adoption time. The controller would synthesize an effective lifecycle
in memory without persisting it to the API server.

**Why rejected:**
- `r.Update(ctx, claim)` at line 1143 writes the full claim object. If the
  synthesized lifecycle is in memory during this write, it leaks to the API
  server. Preventing this requires careful discipline at every code path that
  touches the claim — fragile and error-prone.
- On upgrade, the controller would retroactively apply defaults to existing
  claims with finished workloads, causing immediate deletion. This is the
  same upgrade-safety concern as changing the global default.
- A prior implementation on branch `fix/1306-warmpool-claim-lifecycle` used
  this approach and discovered both issues during review.

### 3. Mutating Admission Webhook - *Deferred*

A mutating webhook that intercepts `SandboxClaim` CREATE requests, looks up
the target pool's `claimDefaults`, and injects the lifecycle before the object
is persisted.

**Pros:**
- Lifecycle is visible immediately on `kubectl get claim -o yaml` — no
  window where `Lifecycle: nil` is visible before the first reconcile.
- Clean separation: webhook handles defaulting, controller handles lifecycle.

**Cons:**
- Agent-sandbox currently has no mutating webhook for claims (only a
  conversion webhook for CRD version conversion). Adding one introduces
  operational complexity (TLS certificates, webhook availability requirements,
  failure mode decisions).
- The controller-side approach achieves the same end state: after the first
  reconcile (which happens within milliseconds of creation), the lifecycle is
  persisted. The brief nil-visibility window is not a practical concern.

**This may be revisited** if the project introduces a mutating webhook for
other purposes (e.g., defaulting other claim fields).

### 4. Place claimDefaults on SandboxTemplate - *Rejected*

Put `claimDefaults` on `SandboxTemplate` instead of `SandboxWarmPool`, so all
pools created from the same template share the same claim defaults.

**Why rejected:**
- `SandboxTemplate` defines the pod spec (image, resources, volumes,
  runtimeClass). Lifecycle is an operational concern, not a workload definition.
- Different pools from the same template may need different lifecycle policies
  (e.g., a dev pool with Retain for debugging vs. a production pool with
  Delete for auto-cleanup).
- The pool is the operational knob. It already controls replicas and update
  strategy. Claim lifecycle defaults belong at the same level.

## Scalability

- **No additional API calls per reconcile.** The pool object is already
  resolved in both the warm adoption and cold fallback paths.
  `resolvePoolLifecycle` receives the already-fetched pool — zero additional
  cache reads.
- **No additional watches.** The claim controller already watches
  `SandboxWarmPool` objects for adoption routing.
- **No impact on non-warm-pool claims.** The injection applies only to claims
  targeting a warm pool via `WarmPoolRef`.
- **No impact on existing pools.** Pools without `claimDefaults` behave
  identically to today. The nil check short-circuits immediately.
