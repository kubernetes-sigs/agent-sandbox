# KEP-729: Opt-in Pod Recreation on PodFailed

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
- [Scalability](#scalability)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

Add an opt-in `Sandbox.spec.podFailurePolicy` field. When set to `Recreate`, the Sandbox controller deletes a controller-owned backing Pod that has entered `phase=Failed`, clears the pod-name annotation, and relies on the existing missing-pod create path to provision a fresh Pod. The Sandbox identity and any PVCs owned by the Sandbox are preserved. The default `Ignore` keeps today's StatefulSet-like behavior.

## Motivation

When a Sandbox's Pod ends in `phase=Failed` (for example after node-pressure eviction by the kubelet), the controller marks `Finished=True/reason=PodFailed` and stops. The Sandbox CR and PVC remain, but no new Pod is created, leaving long-running interactive workloads permanently stuck.

This is distinct from the already-supported case where the Pod object is gone from etcd: with `operatingMode=Running`, the controller recreates a missing Pod. Failed Pods still exist, so that path is never reached.

Core workload precedence:
- **StatefulSet**: leaves Failed pods in etcd (requires user intervention). Agent Sandbox matches this today.
- **ReplicaSet**: ignores Failed pods when counting replicas and creates replacements.

Auto-recreation must remain opt-in so platforms that rely on terminal `Finished` for cleanup (for example Claim `ttlSecondsAfterFinished`) are not broken.

### Goals

- Opt-in recovery from `PodFailed` without deleting the Sandbox or its PVCs.
- Place the control on the Sandbox API (Claim does not manage Pods).
- Preserve default Ignore behavior (non-breaking).

### Non-Goals

- Recreating on `PodSucceeded`.
- SandboxClaim-level passthrough of the policy.
- Template-drift / `podTemplate` update recreation ([#612](https://github.com/kubernetes-sigs/agent-sandbox/issues/612)).
- In-place resource resize ([#1054](https://github.com/kubernetes-sigs/agent-sandbox/issues/1054)).
- Recreate backoff / max-retry limits in the first version.

## Proposal

### User Stories

- As an operator of long-running sandboxes with user data on a PVC, I want a Failed Pod (for example after eviction) to be replaced automatically so the session recovers without data loss.
- As a platform that uses `Finished` + Claim TTL for one-shot jobs, I want the default Ignore behavior so Failed sandboxes still surface `Finished` and can be cleaned up.

### High-Level Design

```text
reconcilePod sees existing Pod
  → if phase=Failed AND podFailurePolicy=Recreate AND owned by Sandbox
      → Delete Pod, clear agents.x-k8s.io/pod-name annotation, return nil
  → next reconcile: Pod missing → existing create path builds a new Pod
  → PVCs from volumeClaimTemplates remain Sandbox-owned and are remounted
```

Expiry handling already short-circuits before child reconcile, so expired Sandboxes do not recreate Failed Pods. Suspend continues to delete the Pod for suspension and does not create while `operatingMode=Suspended`.

With Recreate, `Finished` must not stick on the Failed Pod being replaced: `reconcilePod` runs before `computeFinishedCondition`, and returning a nil Pod after delete clears Finished for that reconcile.

**Crash-loop tradeoff:** `Recreate` combined with `restartPolicy: Never` and a container that always exits non-zero can recreate indefinitely. Document this; backoff is deferred.

#### API Changes

On `SandboxSpec` (runtime field next to `operatingMode`, not in `SandboxBlueprint`):

```go
// PodFailurePolicy controls behavior when the backing Pod reaches phase Failed.
// +kubebuilder:validation:Enum=Ignore;Recreate
type PodFailurePolicy string

const (
	PodFailurePolicyIgnore   PodFailurePolicy = "Ignore"
	PodFailurePolicyRecreate PodFailurePolicy = "Recreate"
)

// podFailurePolicy controls what happens when the backing Pod enters phase Failed.
// Ignore (default): leave the Failed pod and surface Finished=True (StatefulSet-like).
// Recreate: delete the controller-owned Failed pod so a new one is created; PVC/Sandbox retained.
// +kubebuilder:default=Ignore
// +optional
PodFailurePolicy PodFailurePolicy `json:"podFailurePolicy,omitempty"`
```

Enum (not bool) matches project API conventions and `ShutdownPolicy`. A plain enum (not a WarmPool-style `{type:}` struct) is enough because no nested strategy fields are expected.

#### Implementation Guidance

- Modify `reconcileExistingPod` in `controllers/sandbox_controller.go` after ownership is established.
- Only delete when ownership is `resourceOwnedBySandbox` and `DeletionTimestamp` is zero.
- Refuse delete for foreign-owned pods (same logging pattern as suspend).
- Log the recreate delete at `Info` (major lifecycle event).
- Unit-test Ignore vs Recreate, ownership refusal, Succeeded+Recreate (no recreate), and Suspended (suspend path only).
- Optional e2e: Fail once, recreate, remount PVC with surviving data.

## Scalability

Recreate adds at most one Delete + one Create per Failed Pod transition. No new watches, indexes, or unbounded status lists. Controllers that opt many sandboxes into Recreate with crashing workloads may increase Pod churn; that is an operator configuration concern, not a default-path cost.

## Alternatives

| Option | Why it falls short |
| --- | --- |
| `ttlSecondsAfterFinished` + Claim `shutdownPolicy: Delete/Retain` | Deletes Claim and/or Sandbox and can cascade PVC loss. |
| Pod `restartPolicy: Always` | Only restarts containers inside a non-terminal Pod; does not recreate a Failed Pod object. |
| External cron deleting Failed pods | Works via the missing-pod path, but every consumer reimplements it. |
| Claim `lifecycle.recreateOnPodFailure` | Claim does not manage Pods; maintainers prefer Sandbox API. |
| Fold into a broader `updateStrategy` (#612) | Template-drift recreation is a different problem; keep Failed-pod recovery separate. |
