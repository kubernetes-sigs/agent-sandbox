# Threat Model: System Label and Annotation Protection

This document describes a privilege/isolation threat that arises from propagating
user-controlled `PodTemplate` metadata onto the Pods that the Sandbox controller
manages, and the controls that mitigate it.

## Background

A `Sandbox` lets a tenant supply a `spec.podTemplate`, including arbitrary
`metadata.labels` and `metadata.annotations`. The controller propagates that
metadata to the backing Pod so tenants can organize and select their workloads.

The controller and its extensions also rely on a set of **system-reserved**
label and annotation keys to implement core behavior:

- `agents.x-k8s.io/sandbox-name-hash` — the selector label used by the per-Sandbox
  headless `Service`. Traffic for a Sandbox is routed to the Pod(s) carrying the
  matching value.
- `agents.x-k8s.io/sandbox-pod-template-hash`,
  `agents.x-k8s.io/warm-pool-sandbox`,
  `agents.x-k8s.io/sandbox-template-ref-hash` — tracking labels set by the warm
  pool / claim controllers.
- `agents.x-k8s.io/pod-name`, `agents.x-k8s.io/sandbox-template-ref`,
  `agents.x-k8s.io/propagated-labels`, `agents.x-k8s.io/propagated-annotations`,
  and `opentelemetry.io/trace-context` — controller-managed annotations.

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

Related abuses: forging the warm-pool tracking labels to influence
adoption/pooling decisions, or overwriting controller-managed annotations such
as `agents.x-k8s.io/pod-name`.

## Mitigations

The controller treats any label/annotation key under `agents.x-k8s.io/` or
`extensions.agents.x-k8s.io/` (and the trace-context annotation) as
**system-reserved** and never lets user-supplied `PodTemplate` metadata set them:

- **Create path (`reconcilePod`)** and **adoption path (`updatePodMetadata`)**
  filter out system-reserved keys from the user template before applying them.
- The Service selector label `agents.x-k8s.io/sandbox-name-hash` is assigned by
  the controller **after** merging user labels, so it cannot be overridden.
- On adoption/update, system-reserved keys that an older (vulnerable) controller
  recorded in the `propagated-labels` / `propagated-annotations` lists are scrubbed
  from the Pod — except the controller-owned name-hash label, the allowed tracking
  labels on extension-managed Sandboxes, and the controller-managed annotations
  (`propagated-labels`, `propagated-annotations`, and the trace-context annotation).
  Combined with always (re)setting the name-hash label to the controller's value,
  this prevents a stale or spoofed Service-selector label from surviving adoption.
- The warm-pool tracking labels are **only** propagated for Sandboxes that are
  owned by a trusted extension controller (`SandboxWarmPool` / `SandboxClaim`) via a
  *controller* owner reference. This assumes the
  `OwnerReferencesPermissionEnforcement` admission plugin is active so a tenant
  cannot attach such an owner reference to an object they cannot delete; verifying
  the owner object's existence and UID would harden this further.

## Out of scope

- The value of the name hash is still derived with FNV-1a. The label-protection
  controls above hold regardless of the hash algorithm; strengthening the hash
  (e.g. to a truncated SHA-256) is tracked separately.
- Network policy is the primary, defense-in-depth control for tenant isolation;
  this mitigation removes a control-plane bypass of the Service-based routing.
