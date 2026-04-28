# Sandbox CRD — API Behavior Reference

This document records the **current, authoritative behavior** of the `Sandbox` CRD and its extensions
(`SandboxTemplate`, `SandboxWarmPool`). It focuses on behaviors that are not immediately obvious from
reading the field descriptions alone, and flags open design questions that are tracked as follow-up issues.

> **Status:** v1alpha1 — behaviors are subject to change.

---

## Table of Contents

- [`.spec.replicas`](#specreplicas)
- [`.spec.podTemplate`](#specpodtemplate)
- [`.spec.volumeClaimTemplates`](#specvolumeclaimtemplates)
- [SandboxTemplate and WarmPool](#sandboxtemplate-and-warmpool)
- [Open Design Questions](#open-design-questions)

---

## `.spec.replicas`

`replicas` is an integer field constrained to `0` or `1` (default `1`). It is also exposed via
the `/scale` subresource, so standard autoscalers and `kubectl scale` work against it.

### Apparent behaviors

**Setting `replicas: 0` deletes the Pod.**  
When the controller reconciles a Sandbox with `replicas: 0`, it finds the owned Pod and issues a
`Delete`. If the Pod already has a `DeletionTimestamp`, the controller does not re-issue the delete;
instead, it treats the sandbox as already scaled down for reconcile purposes. The Pod name annotation
(`agents.x-k8s.io/pod-name`) is also cleared from the Sandbox at this point.

When `replicas: 0`, the Sandbox `Ready` condition is set to `True` with reason `DependenciesReady`.
The absence of an active Pod — including the case where the previous Pod is still terminating — is
treated as the **intended state**, not an error.

### Non-apparent behaviors

**PVCs created from `volumeClaimTemplates` are preserved when `replicas` is set to `0`.**  
The controller never deletes PVCs during a scale-down. Only the Pod (and, on expiry, the Service)
are deleted. PVCs outlive the Pod and remain bound to their underlying PersistentVolumes.

**When `replicas` is set back to `1`, the previously created PVCs are re-attached.**  
On scale-up the controller runs `reconcilePVCs` before creating the Pod. Because the PVCs already
exist and are owned by the Sandbox, they are left as-is. The new Pod is then created with volume
mounts pointing to those same PVCs (using the naming convention `{template.name}-{sandbox.name}`).
The result is that the filesystem context inside the container's mounted paths is fully restored —
the agent picks up exactly where it left off.

This makes the `replicas` field a lightweight **save/restore primitive for filesystem state**.
An agent platform can pause a sandbox (replicas → 0), free the node resource, and resume it later
(replicas → 1) with the working directory intact. Future work (memory snapshots, pod root filesystem
archival — see [Open Design Questions](#open-design-questions)) would complement this model.

**Ownership guards apply.** The controller only deletes or adopts resources it owns. Ownership
conflicts are **not** surfaced uniformly in status across all reconcile paths. In particular, when
`replicas: 0`, an unowned Pod or a Pod owned by a different controller is logged and skipped during
the scale-down delete path, and reconciliation can still report `Ready=True` because no Pod is the
intended state. By contrast, conflicting ownership on resources reconciled as dependencies (for
example, Service/PVC paths) may cause reconcile to return an error, which is then reflected in
Sandbox status.

### Scale via `/scale` subresource

The `/scale` subresource maps to `spec.replicas` / `status.replicas` / `status.selector`. All of
the behaviors above apply equally whether `replicas` is changed directly on the Sandbox spec or
through the scale subresource.

---

## `.spec.podTemplate`

`podTemplate` has two sub-fields:

| Sub-field | Purpose |
|---|---|
| `podTemplate.metadata` | Labels and annotations propagated to the Pod |
| `podTemplate.spec` | The full `PodSpec` used to create the Pod |

### Non-apparent behaviors

**`podTemplate.metadata` changes are propagated to the running Pod immediately.**  
When the controller reconciles a Sandbox that already has a running Pod, it calls `updatePodMetadata`,
which diffs the desired labels/annotations from `podTemplate.metadata` against what is currently on
the Pod and issues a `Pod.Update` if anything changed. This update does **not** restart the container.

Deletion of a label or annotation from `podTemplate.metadata` is also handled: the controller tracks
which keys it previously propagated using the following Pod annotations —

- `agents.x-k8s.io/propagated-labels` — comma-separated list of label keys last propagated from the template  
- `agents.x-k8s.io/propagated-annotations` — comma-separated list of annotation keys last propagated from the template

Keys that appear in these tracking annotations but are absent from the current `podTemplate.metadata`
are removed from the Pod. Keys that were added to the Pod by external sources (e.g. mutating
webhooks) are not touched.

**`podTemplate.spec` changes do NOT affect a running Pod.**  
The controller does not patch or replace the spec of an existing Pod. A TODO comment in
`reconcileExistingPod` explicitly defers this decision. Spec changes take effect only when the
existing Pod is deleted — at that point the controller creates a new Pod from the current
`podTemplate.spec`.

To apply a spec change to a running Sandbox, delete the tracked Pod manually. Do not assume the Pod
name always matches the Sandbox name. Instead, delete by the tracked pod name annotation on the
Sandbox, or by the controller-provided selector in `.status.selector`:

```bash
# Option 1: delete the tracked Pod by name
kubectl delete pod "$(kubectl get sandbox <sandbox-name> -n <namespace> \
  -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')" -n <namespace>

# Option 2: delete using the Pod label selector surfaced in status
kubectl delete pod -l "$(kubectl get sandbox <sandbox-name> -n <namespace> \
  -o jsonpath='{.status.selector}')" -n <namespace>
```

The controller will immediately create a replacement Pod from the updated `podTemplate.spec`.

> **⚠ Open question:** Should the controller support in-place spec updates, or enforce a delete-and-recreate
> policy? Volumes are a special consideration — see [Open Design Questions](#open-design-questions).

**Pod name may differ from Sandbox name when a WarmPool pod is adopted.**  
When a pre-warmed Pod is adopted from a `SandboxWarmPool`, its original name is tracked in the
`agents.x-k8s.io/pod-name` annotation on the Sandbox. The controller uses this annotation to
locate the Pod on all subsequent reconcile loops. If the annotated Pod is not found (e.g. it was
deleted externally), the annotation is cleared and the controller falls back to the Sandbox name to
look up or create a Pod.

---

## `.spec.volumeClaimTemplates`

`volumeClaimTemplates` is a list of PVC templates. Each entry drives the creation of one
PersistentVolumeClaim per Sandbox.

### PVC naming

PVCs are named `{template.metadata.name}-{sandbox.name}`. For example, a template entry named
`data` in a Sandbox named `my-sandbox` produces a PVC named `data-my-sandbox`.

### Non-apparent behaviors

**`volumeClaimTemplates` entries only drive PVC _creation_, never _update_ or _deletion_.**  
Once a PVC exists and is owned by the Sandbox, the controller takes no further action on it
regardless of changes to the `volumeClaimTemplates` spec. Changes to the template (storage class,
access modes, capacity) are silently ignored for already-existing PVCs, since PVC specs are largely
immutable in Kubernetes after binding.

**Adding a new entry to `volumeClaimTemplates` creates a new PVC and attaches it on the next Pod
recreation.** The PVC is provisioned immediately (on the next reconcile), but it does not appear as
a volume in the running Pod until the Pod is deleted and recreated.

**Removing an entry from `volumeClaimTemplates` does NOT delete the corresponding PVC.**  
Orphaned PVCs are left in place. They are no longer mounted by new Pods (because the volume
injection logic is driven by the current `volumeClaimTemplates` list), but they continue to exist
and retain their data. Manual deletion is required to reclaim storage.

**Volume injection follows StatefulSet semantics.**  
When creating a Pod, the controller builds a PVC-backed volume for each entry in
`volumeClaimTemplates` and merges them into `podTemplate.spec.volumes` via `MergeVolumeClaimVolumes`.
Entries in `podTemplate.spec.volumes` with the same name as a `volumeClaimTemplates` entry are
replaced by the PVC-backed version. This mirrors StatefulSet behavior: volumeClaimTemplate-derived
volumes take priority over inline volumes of the same name.

**Unowned PVCs matching the expected name are adopted.**  
If a PVC with the correct name exists but has no `ownerReference`, the controller sets itself as the
owner. This supports recovery scenarios where a PVC was pre-created or became orphaned.

> **⚠ Open question:** What should happen to PVCs created from removed `volumeClaimTemplates` entries?
> Should the controller offer a `reclaimPolicy` field to optionally delete them? See
> [Open Design Questions](#open-design-questions).

---

## SandboxTemplate and WarmPool

### SandboxTemplate

A `SandboxTemplate` defines a reusable `podTemplate` and `volumeClaimTemplates` that `SandboxClaim`
and `SandboxWarmPool` objects reference.

**What happens when `podTemplate` in a SandboxTemplate is changed?**  
The `SandboxTemplate` itself is just a configuration object, but changes to it can affect
downstream objects differently depending on which part of `podTemplate` changed and how the
template is referenced:

- **Sandboxes created directly from a claim:** existing Sandboxes are not fully rebuilt just
  because the template changed, but the `SandboxClaim` controller does resync
  `spec.podTemplate.metadata` from the template (plus any claim-level overrides) onto the
  existing `Sandbox`. That means metadata changes such as labels/annotations can propagate to
  the running Pod via the core `Sandbox` controller's in-place metadata update path. By
  contrast, changes to `spec.podTemplate.spec` do **not** update an already-running Pod in
  place; those changes only take effect for newly created Sandboxes or after Pod recreation.

- **WarmPool pods:** the `SandboxWarmPool` controller detects that the pool's pods no longer match
  the current template (tracked via `agents.x-k8s.io/sandbox-pod-template-hash` label) and responds
  according to its `updateStrategy` (see below).

### SandboxWarmPool

A `SandboxWarmPool` maintains a pool of pre-warmed Sandbox Pods ready for immediate adoption by
incoming `SandboxClaim` requests.

**`updateStrategy` controls how stale pool pods are handled after a template change.**

| Strategy | Behavior |
|---|---|
| `OnReplenish` *(default)* | Stale pods are left running. They are only replaced when manually deleted or when they are adopted by a `SandboxClaim` and a fresh pod is created to replenish the pool. |
| `Recreate` | Stale pods are deleted immediately when the pool is stale due to `podTemplate.spec` drift, so the pool converges to the current template as those entries are recreated. **Note:** This applies to `podTemplate.spec` changes only. Changes to `podTemplate.metadata` (labels/annotations) do not trigger a Recreate cycle and do not update existing pool sandboxes/pods in place. |

**Metadata-only template changes are not applied to existing pool entries.**  
Regardless of `updateStrategy`, changes that affect only `podTemplate.metadata` (labels/annotations)
in the SandboxTemplate do not cause pool sandboxes/pods to be deleted and recreated, and they are
not retroactively applied to existing pool entries. Those metadata changes only take effect for pool
sandboxes/pods created after an existing entry is replaced or the pool is replenished.

**`volumeClaimTemplates` changes in a SandboxTemplate affect only newly created pool pods.**  
Existing warm pool pods are backed by PVCs that were provisioned at creation time. A template change
to `volumeClaimTemplates` is not retroactively applied to the running pool pods or their PVCs.

---

## Open Design Questions

The following questions were surfaced during the gap analysis by @bowei. Each should be tracked as
a separate GitHub issue.

### [Issue] Allow complete mutability of `podTemplate`?

**Current behavior:** Only `podTemplate.metadata` changes are applied in-place to a running Pod.
`podTemplate.spec` changes require the Pod to be deleted and recreated.

**Questions to resolve:**
- Should the controller support automatic rolling restarts when `podTemplate.spec` changes?
- Are there restrictions on which spec fields can be mutated between Pod incarnations? Volumes are
  a specific concern: if a volume definition in `podTemplate.spec.volumes` conflicts with a
  `volumeClaimTemplates`-derived volume on the replacement Pod, the behavior is currently undefined.
- Should there be an explicit `restartPolicy`-style field on the Sandbox to control this, similar
  to `SandboxWarmPool.updateStrategy`?

### [Issue] Allow complete mutability of `volumeClaimTemplates`?

**Current behavior:** Adding entries creates new PVCs. Removing entries orphans existing PVCs (they
are not deleted). Modifying entries for existing PVCs has no effect.

**Questions to resolve:**
- Should the controller support a reclaim policy (e.g. `delete` vs `retain`) for PVCs belonging to
  removed `volumeClaimTemplates` entries?
- When scaling down (`replicas: 0`), should users have the option to also delete PVCs? This would
  trade the "context restore on scale-up" feature for storage cost savings.
- What happens if `volumeClaimTemplates` entries are added or removed while `replicas: 0`? The PVCs
  are created immediately, but there is no running Pod to validate the mount paths against.

### [Issue] Snapshotting beyond filesystem: memory and root filesystem

**Current behavior:** The `replicas: 0 → 1` cycle preserves and restores PVC-backed filesystem
state. Memory and the pod's root (ephemeral) filesystem are not preserved.

**Potential enhancements:**
- Integration with VolumeSnapshot APIs to snapshot PVC state at scale-down.
- Support for CRIU-based memory checkpointing when a compatible runtime is present.
- Archival of the pod root filesystem to a persistent volume or object store.

### [Issue] Clarify `podTemplate` changes impact on SandboxTemplate warm pool pods

Needs a full lifecycle matrix documenting exactly which field changes in a `SandboxTemplate` trigger
pod replacement in a `SandboxWarmPool` under each `updateStrategy`, including edge cases where pods
are mid-adoption when the template changes.
