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
with `Lifecycle: nil` is adopted from a pool that has `claimDefaults.lifecycle`
configured, the controller copies the pool's lifecycle into the claim at
adoption time. Claims that set their own `Lifecycle` are never modified.

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
  sandboxes are unnecessary attack surface that `Delete + TTL=0` eliminates.

### The production viability question

Without a mechanism for pool operators to enforce claim cleanup, **VM-backed
warm pools are not viable in production.** The issue is not theoretical:

- Benchmark data on a 30-replica kata-qemu pool shows pool starvation after
  ~90 claims under sustained load with nil-lifecycle claims ([#1306][1306]).
- Every SDK crash, network partition, or agent that omits `Close()` leaves a
  VM running indefinitely — consuming real CPU, memory, and hypervisor
  processes.
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
2. Apply defaults only at claim creation time (adoption), never retroactively.
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
      ttlSecondsAfterFinished: 0
```

Any `SandboxClaim` targeting `kata-agent-pool` with `Lifecycle: nil` will
inherit `Delete + TTL=0`. A claim that explicitly sets `Retain` keeps its
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
| Operator adds `claimDefaults` to a pool with existing `Lifecycle: nil` claims whose workloads have finished | Defaults are applied **only during adoption** (create-time). Existing claims are already adopted and never re-enter the adoption path. No retroactive deletion. |
| `claimDefaults` lost during v1alpha1 round-trip | State-preservation annotation on the v1alpha1 object stores the serialized `ClaimDefaults`, restored on conversion back to v1beta1. Covered by round-trip test. |
| Confusion about which lifecycle applies | The claim's `Spec.Lifecycle` is always the source of truth. `claimDefaults` is copied into it at creation — after adoption, `kubectl get sandboxclaim -o yaml` shows the effective lifecycle directly. No indirection. |
| Suspended sandbox auto-deleted by injected TTL | `TTLSecondsAfterFinished` counts from workload completion, not from adoption. A sandbox in `operatingMode: Suspended` ([KEP-694][694]) is intentionally on hold — it has not finished. The expiration controller (`checkExpiration`) must treat `Suspended` as a non-terminal state and not start the TTL clock. This is KEP-694's responsibility; `claimDefaults` only sets the values, it does not alter expiration semantics. |

## Design Details

### API Changes

Add `ClaimDefaults` to `SandboxWarmPoolSpec` in
`extensions/api/v1beta1/sandboxwarmpool_types.go`:

```go
type SandboxWarmPoolSpec struct {
    // ...existing fields (Replicas, TemplateRef, UpdateStrategy)...

    // claimDefaults specifies default values applied to SandboxClaims that
    // target this pool and do not set the corresponding fields themselves.
    // Defaults are applied at claim creation time (during adoption) and are
    // not retroactively applied to existing claims.
    // +optional
    ClaimDefaults *ClaimDefaults `json:"claimDefaults,omitempty"`
}

// ClaimDefaults defines default values for SandboxClaims adopted from a pool.
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

**Note:** `Lifecycle` also includes a `ShutdownTime` field (absolute
timestamp). `ShutdownTime` is **not recommended** in `claimDefaults` because
all adopted claims would share the same absolute deadline — claims adopted
after that time would expire immediately. Implementers should add a
validating webhook or CEL rule rejecting `ShutdownTime` in `claimDefaults`.
Only `ShutdownPolicy` and `TTLSecondsAfterFinished` are meaningful as pool-
level defaults.

### Controller Implementation

The injection point is `adoptSandboxFromCandidates` in
`extensions/controllers/sandboxclaim_controller.go`, immediately before the
`r.Update(ctx, claim)` call that records the `AssignedSandboxName` annotation
(line ~1143). This is the single atomic write during adoption:

```go
// Resolve pool's claimDefaults once, before the adoption write.
var poolLifecycle *extensionsv1beta1.Lifecycle
if claim.Spec.Lifecycle == nil {
    pool := &extensionsv1beta1.SandboxWarmPool{}
    poolKey := types.NamespacedName{
        Name:      claim.Spec.WarmPoolRef.Name,
        Namespace: claim.Namespace,
    }
    if err := r.Get(ctx, poolKey, pool); err == nil {
        if pool.Spec.ClaimDefaults != nil && pool.Spec.ClaimDefaults.Lifecycle != nil {
            poolLifecycle = pool.Spec.ClaimDefaults.Lifecycle.DeepCopy()
        }
    }
}

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

1. Adoption normally happens once per claim. The `AssignedSandboxNameAnnotation`
   is set during adoption and checked before re-entering the adoption path.
   However, if the assigned sandbox is lost (deleted, not found, or owned by
   another claim), the controller removes the annotation
   (`removeAssignedSandboxReference`) and the claim re-enters adoption. On
   re-adoption, the nil check (`fresh.Spec.Lifecycle == nil`) prevents
   re-injection if the previous adoption already persisted a lifecycle — the
   injected lifecycle survives annotation removal because only annotations and
   labels are cleared, not spec fields.
2. The `r.Update` at this point is a full-object PUT. The lifecycle is persisted
   to the API server atomically with the adoption annotation.
3. On conflict retry (`retryAdoptionWithDefaults`), the claim is re-read fresh
   from the API server. The retry callback re-applies both the annotation and
   the lifecycle default (if the fresh object still has `Lifecycle: nil`). The
   `poolLifecycle` is resolved once before the first attempt and reused across
   retries — no repeated pool lookups.
4. No other code path in the controller writes to `claim.Spec.Lifecycle`. All
   2 `r.Update` and 8 `r.Patch` call sites on claims modify only annotations,
   labels, or the status subresource.

**Pool lookup cost:**

The pool is already cached by the informer. The `r.Get` reads from the local
cache, not the API server. The adoption path already performs multiple cache
reads (sandbox lookup, template lookup), so one additional read is negligible.

**Bare sandbox creation (non-warm-pool):**

When a claim creates a sandbox directly (pool has 0 replicas or the claim
bypasses the pool), the `createSandbox` path does not call `r.Update` on the
claim. `claimDefaults` does not apply in this path, which is correct: bare
creation is an explicit, operator-controlled flow where the claim author is
expected to set lifecycle directly.

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

This ensures lossless round-tripping in both directions: a v1beta1 pool with
`claimDefaults` read via v1alpha1 and written back retains the field, and a
v1beta1-originated pool updated through the v1alpha1 API preserves
`ClaimDefaults`.

### Test Plan

**Unit tests** (`extensions/controllers/sandboxclaim_controller_test.go`):

| Test case | Setup | Expected |
|-----------|-------|----------|
| `warm + nil lifecycle + claimDefaults` | Pool has `Delete+TTL=0` defaults, claim has `Lifecycle: nil` | Claim adopted with `Delete+TTL=0` lifecycle. `kubectl get claim` shows lifecycle. |
| `warm + explicit Retain + claimDefaults` | Pool has `Delete+TTL=0` defaults, claim has explicit `Retain` | Claim lifecycle unchanged. Pool defaults ignored. |
| `warm + nil lifecycle + no claimDefaults` | Pool has no `claimDefaults`, claim has `Lifecycle: nil` | Claim adopted with `Lifecycle: nil`. Behavior identical to today. |
| `warm + nil lifecycle + pool not found` | Pool deleted before adoption completes | Existing `ErrWarmPoolNotFound` handling. No crash. |
| `cold fallback + nil lifecycle` | No ready candidate in pool, claim falls through to `createSandbox` | Lifecycle remains nil. `claimDefaults` not applied on the cold path. |

**Conversion tests** (`extensions/api/v1alpha1/sandboxwarmpool_conversion_test.go`):

| Test case | Expected |
|-----------|----------|
| Round-trip with `ClaimDefaults` populated | v1beta1 → v1alpha1 → v1beta1 preserves `ClaimDefaults` via state annotation |
| Round-trip with `ClaimDefaults` nil | No v1beta1 state annotation on the v1alpha1 object. Restored v1beta1 has nil `ClaimDefaults`. Identical to current behavior. |

**E2E tests** (if appropriate for scope):

- Create pool with `claimDefaults.lifecycle: Delete+TTL=0`, create claim with
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
  `claimDefaults` and adopts with nil lifecycle. The new version applies
  defaults. This is acceptable: the window is brief, and the worst case is
  a claim with Retain behavior (the current default) during the rollout.

## Alternatives Considered

### 1. Change the Global Default to Delete - *Rejected*

Change the default `ShutdownPolicy` from `Retain` to `Delete` for all
`SandboxClaim` objects.

**Why rejected:**
- Breaking change. Existing claims with `Lifecycle: nil` would be retroactively
  deleted on controller upgrade.
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

- **No additional API calls per reconcile.** The pool `r.Get` during adoption
  reads from the local informer cache. Adoption already performs multiple
  cache reads; one additional read is negligible.
- **No additional watches.** The claim controller already watches
  `SandboxWarmPool` objects for adoption routing.
- **No impact on non-warm-pool claims.** The injection is gated on the
  adoption path, which is only entered for warm pool claims.
- **No impact on existing pools.** Pools without `claimDefaults` behave
  identically to today. The nil check short-circuits immediately.
