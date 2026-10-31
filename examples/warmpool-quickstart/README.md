# Warm Pool Quickstart

This directory contains reference YAML for the three extension CRDs that power
the warm-pool workflow: `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`.

## Files

*   `sandboxtemplate.yaml`: Admin-owned blueprint for sandboxes, with persistent storage via `volumeClaimTemplates`.
*   `sandboxwarmpool.yaml`: Pre-warms N sandboxes to avoid cold-start costs.
*   `sandbox-claim.yaml`: User-facing claim to adopt a sandbox from the pool.
*   `secure-sandboxtemplate.yaml`: Hardened template with gVisor runtime and tight NetworkPolicy (DNS-only egress, ingress locked to the Istio ingress gateway).
*   `llm.yaml`: Template that grants access to a private LLM endpoint via `hostAliases` and a scoped egress NetworkPolicy.

## Choosing a standby policy

By default, `spec.replicas` is the number of unclaimed standby Sandboxes the pool
maintains. Claiming a Sandbox removes it from that inventory, so the controller
replenishes the pool. For example, `replicas: 100` can mean 100 claimed Sandboxes
plus another 100 standby Sandboxes. The supplied `sandboxwarmpool.yaml` keeps
this fixed-standby behavior.

For workloads that hold a batch of Sandboxes for a long time, set
`spec.replenishmentCapacity` when creating a **new** pool to reduce standby as
the pool's Sandboxes are claimed:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: training-env-pool
  namespace: default
spec:
  replicas: 100
  replenishmentCapacity: 100
  sandboxTemplateRef:
    name: secure-datascience-template
```

The referenced `SandboxTemplate` must exist in the same namespace. With R as
`replicas` (default 1), B as `replenishmentCapacity`, and C as the number of
occupied Sandboxes, the controller computes the standby target T:

```text
replenishmentCapacity unset: T = R
replenishmentCapacity set:   T = min(R, max(0, B - C))
```

In the opt-in mode, R is a standby ceiling. B can be smaller or larger than R;
`replenishmentCapacity: 0` means a zero standby target, not unlimited capacity
or a return to the default policy. Both values must be non-negative, and
`replicas: 0` also means zero standby.

For R = B = 100:

| Occupied Sandboxes still present | Standby target |
| --- | --- |
| 0 | 100 |
| 40 | 60 |
| 100 | 0 |
| 70, after deleting 30 claimed Sandboxes | 30 |
| 120 | 0 |

This trades resource use for the next request's startup latency. It is **not a
hard limit** on Sandbox count, Pod count, or compute resources: claims still
fall back to cold creation when no suitable warm Sandbox is available, even
when C exceeds B. Reconciliation, cache observation, and deletion take time,
so occupied plus standby can temporarily exceed B. Existing refill pacing and
batch limits still apply.

The presence of `replenishmentCapacity` is immutable after creation. Its value
and `replicas` can be updated, including B = 100 → 0 → 100, but adding the field
to an existing fixed-standby pool or removing it (including setting it to
`null`) is rejected. Choose the policy when creating the pool.

### What counts as occupied

The controller records the source pool's name and UID on Sandboxes. The UID
distinguishes a pool from a later pool with the same name. Occupied means a
Sandbox from that pool still exists but is no longer controlled as its standby
inventory. This includes warm adoptions and pool-associated cold creations,
including claims with custom environment variables or volume configuration.
It also includes same-source Sandboxes that are orphaned or controlled by
another owner; they are not automatically adopted back into standby or deleted
by the pool.

Readiness, suspension, or command completion does not release capacity. A
terminating claimed Sandbox continues to count until its Sandbox resource is
gone, even if its Pod has already stopped. A retained Claim with no remaining
Sandbox does not count. Multiple consumers use the same pool without keeping a
shared counter or patching its configuration after each claim.

Do not edit or remove the controller's source annotations. If a related
Sandbox's source cannot be established safely, the pool pauses replenishment
and inventory cleanup, emits a `WarmPoolAccountingIncomplete` Warning Event,
and clears the two occupancy-related status fields. It resumes when the source
can be established. Legacy standby still owned by the current pool can have
its source repaired from that owner; a claimed Sandbox with only a same-name
Claim reference cannot. If every source and ownership/link record is erased,
the original pool cannot be recovered; this metadata is not a security boundary
or a hard quota.

### Reading status and waiting for warm capacity

In the opt-in mode, `status.occupiedReplicas` reports C and
`status.desiredStandbyReplicas` reports T after a complete accounting
observation. Both fields are absent in fixed-standby mode or when accounting is
unavailable; an explicit `0` is a successful observation of zero, not an unknown
value. `status.replicas` and `status.readyReplicas` continue to describe standby
inventory, and the existing `/scale` fields and selector retain their meanings.

Status is an observed snapshot, not a real-time guarantee: writes or reads can
fail, leaving an older server-side value. `status.observedGeneration` tracks
configuration changes, not each occupancy change. Wait for the expected
occupied count, target, and actual standby/Ready counts together rather than
using `observedGeneration` alone.

A pool can converge to T = 0 without any warm Sandbox to claim. If the caller
requires a warm launch, check `status.readyReplicas >= 1`; otherwise the next
claim may be cold. Existing fixed-standby SDK/RL readiness waiters are not
automatically adapted to this policy.

### Scaling, upgrades, and returning to fixed standby

The opt-in mode currently supports manually configured R and B only. Do not
combine it with HPA or KEDA. The `/scale` endpoint remains available, but that
does not imply autoscaler compatibility, and the controller does not reject or
automatically remove an autoscaler. Fixed-standby pools retain their existing
scaling behavior.

Update the CRDs first, then finish rolling out the new controller and confirm
its readiness **before** creating an opt-in pool. An old CRD may prune the
field and an old controller may ignore it; enabling the mode during a mixed
controller rollout is unsupported.

Rolling back to an old controller while opt-in pools exist is unsupported.
Setting B to zero is not a rollback switch. To return to fixed standby, create
a new pool without `replenishmentCapacity`, preferably with a different name,
then explicitly decide how existing claims and Sandboxes will be handled.
There is no automatic migration or deletion of in-use Sandboxes.

## Related examples

*   **[kata-aks](../kata-aks/)**: Full end-to-end example — Python agent, Go client, and sandbox-router — on AKS with Kata Containers (Hyper-V) isolation.
