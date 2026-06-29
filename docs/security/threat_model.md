# Threat Model: System Label and Annotation Protection

This document describes a privilege/isolation threat that arises from propagating
user-controlled `PodTemplate` metadata onto the Pods that the Sandbox controller
manages, and the controls that mitigate it.

## Background

A `Sandbox` lets a tenant supply a `spec.podTemplate`, including arbitrary
`metadata.labels` and `metadata.annotations`. The core controller propagates that
metadata to the backing Pod so tenants can organize and select their workloads.

The controller also relies on a set of **system-reserved** label and annotation
keys to implement core behavior:

- `agents.x-k8s.io/sandbox-name-hash` — the selector label used by the per-Sandbox
  headless `Service`. Traffic for a Sandbox is routed to the Pod(s) carrying the
  matching value.
- `agents.x-k8s.io/propagated-labels`, `agents.x-k8s.io/propagated-annotations`,
  and `opentelemetry.io/trace-context` — controller-managed annotations.

Extension controllers (warm pool, claim) may set additional system-prefixed labels
on the **Sandbox CR** (`metadata.labels`, `spec.podTemplate`, etc.). The core
Sandbox reconciler does not propagate those to Pods; extension controllers own
that lifecycle separately.

## Threat

**Spoofing / cross-tenant traffic hijack via reserved-key injection.**

If user-supplied template metadata is propagated verbatim, a tenant can set a
system-reserved key to a value of their choosing. The highest-impact case is the
Service selector label:

1. Tenant A creates `Sandbox A`; its Service selects Pods labeled
   `agents.x-k8s.io/sandbox-name-hash=<hash(A)>`.
2. Tenant B (malicious) creates `Sandbox B` with
   `spec.podTemplate.metadata.labels["agents.x-k8s.io/sandbox-name-hash"] = <hash(A)>`.
3. Tenant B's Pod now also matches Sandbox A's Service selector, so traffic
   intended for Sandbox A can be delivered to the attacker's Pod
   (a network-isolation bypass / traffic-hijack primitive).

Related abuses: forging system-prefixed labels or overwriting controller-managed
annotations such as `agents.x-k8s.io/pod-name`.

## Mitigations

The core controller treats any label/annotation key under `agents.x-k8s.io/` or
`extensions.agents.x-k8s.io/` (and the trace-context annotation) as
**system-reserved** and never lets user-supplied `PodTemplate` metadata set them:

- **Create path (`reconcilePod`)** and **adoption path (`updatePodMetadata`)**
  filter out system-reserved keys from the user template before applying them.
- The Service selector label `agents.x-k8s.io/sandbox-name-hash` is assigned by
  the controller **after** merging user labels, so it cannot be overridden.
- On adoption/update, system-reserved keys that an older (vulnerable) controller
  recorded in the `propagated-labels` / `propagated-annotations` lists are scrubbed
  from the Pod — except the controller-owned name-hash label and the
  controller-managed annotations (`propagated-labels`, `propagated-annotations`).
  Combined with always (re)setting the name-hash label to the controller's value,
  this prevents a stale or spoofed Service-selector label from surviving adoption.
- System labels on `Sandbox.metadata.labels` are **not** copied to Pods by the
  core controller. Only non-system keys from `spec.podTemplate` are propagated.

## Out of scope

- Extension controllers manage their own labels on Sandbox CRs and may patch Pod
  metadata through separate reconciliation paths. The core controller intentionally
  does not encode extension owner-reference or warm-pool tracking logic.
- The value of the name hash is still derived with FNV-1a. The label-protection
  controls above hold regardless of the hash algorithm; strengthening the hash
  (e.g. to a truncated SHA-256) is tracked separately.
- Network policy is the primary, defense-in-depth control for tenant isolation;
  this mitigation removes a control-plane bypass of the Service-based routing.
