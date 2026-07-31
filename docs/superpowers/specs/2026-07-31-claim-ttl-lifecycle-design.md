# Claim TTL Lifecycle Design

**Goal:** Make `SandboxClaim.spec.ttlSecondsAfterCreated` initialize the existing lifecycle shutdown flow instead of running a separate claim-age deletion path.

**Scope:** SandboxClaim only. SandboxWarmPool continues to use its independent age-based TTL because it has no lifecycle API.

## Behavior

- `ttlSecondsAfterCreated` remains an optional API field and remains preserved by v1alpha1/v1beta1 conversions.
- When a claim has a TTL and `spec.lifecycle.shutdownTime` is unset, the reconciler patches `shutdownTime` to `metadata.creationTimestamp + TTL`.
- When the claim has no lifecycle, the reconciler initializes one with that derived shutdown time and `shutdownPolicy: Delete`; this preserves the existing TTL promise that the claim itself is removed.
- When a lifecycle already exists with no shutdown time, the reconciler fills only the derived shutdown time and preserves its configured shutdown policy.
- When a shutdown time is explicitly set, it is authoritative and is never overwritten by the TTL-derived time.
- After initialization, expiration, status updates, events, requeues, and claim deletion run through the existing lifecycle code path. The separate claim age-TTL delete/requeue block is removed.

## Patch Safety

- Derive the shutdown time only from persisted `creationTimestamp` and the TTL value.
- Use a merge patch from a deep copy so an informer-cached object is not mutated before persistence.
- Make initialization idempotent: later reconciliations observe the persisted shutdown time and do not patch again.
- Do not modify warm-pool TTL behavior.

## Regression Coverage

- TTL-only claims receive a derived lifecycle shutdown time and delete through lifecycle policy handling once expired.
- A lifecycle without shutdown time receives the derived timestamp while retaining its shutdown policy.
- An explicit lifecycle shutdown time remains unchanged when TTL is also configured.
- Reconciliation remains idempotent after derived shutdown time is persisted.
- Existing conversion and event-recorder coverage remains valid.

## Validation

- Run focused extension controller tests and conversion tests.
- Run generated API/docs checks if source or API markers require updates.
- Rerun required Prow presubmits after push.
