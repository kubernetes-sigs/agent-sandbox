# Proposal: Isolation-Tier Routing for SandboxClaims

**Status**: Draft Proposal
**Authors**: @vvoronko
**Created**: 2026-07-14
**Updated**: 2026-07-23

## Summary

This proposal introduces a mutating admission webhook that routes `SandboxClaim`
resources to the appropriate `SandboxWarmPool` based on isolation-tier annotations.
Instead of clients hardcoding a pool name, they declare the isolation level they need
(`process` or `hardware`) and whether fallback across tiers is acceptable. The webhook
rewrites `spec.warmPoolRef.name` at admission time, keeping the existing controller
and CRD surface unchanged.

## Motivation

### Warm claims are runtime-independent

Benchmark data from PR #1262 (3 runtimes, 6 pool sizes, n2-standard-8 GCP workers)
shows that **warm claim latency is identical across runtimes**:

| Runtime | Warm P50 | Warm P95 | Warm P99 | Calibration baseline |
|---------|----------|----------|----------|---------------------|
| runc    | 0.322s   | 0.483s   | 0.487s   | 0.317s |
| gVisor  | 0.320s   | 0.488s   | 0.490s   | 0.319s |
| kata    | 0.328s   | 0.471s   | 0.472s   | 0.320s |

*(pool=8, burst recovery benchmark, n2-standard-8 GCP workers)*

The ~0.32s baseline is the controller reconcile path, not the runtime. Warm claims
from any runtime cross the Ready condition in effectively the same time.

### Cold starts differ by orders of magnitude

When a pool is exhausted, cold start penalties diverge dramatically:

| Runtime | Cold start observed? | Representative cold latency |
|---------|---------------------|-----------------------------|
| runc    | No (pool always kept up) | ~0.32s expected (no VM boot) |
| gVisor  | 1 claim at pool=4 | 1.115s |
| kata    | 24 claims across pool sizes | 8.0-13.5s (increases with contention) |

runc cold starts have no VM overhead and are expected to be indistinguishable from
warm. gVisor's single observed cold start (1.1s) crosses the 1s UX boundary. kata
cold starts range from 8s (low contention) to 13.5s (high contention) — pool
exhaustion is catastrophic for user experience.

### Pool sizing is the real lever

Throughput is capped by the controller's work-queue serialization at ~10 claims/sec for
runc and gVisor, ~2 claims/sec for kata. The right pool size absorbs burst traffic
within the warm budget; the wrong size forces cold starts regardless of runtime.

### The routing problem

Today, `SandboxClaim.spec.warmPoolRef` is required and points to a single pool.
Operators running mixed isolation environments (e.g., gVisor for general workloads,
kata for untrusted code execution) face a choice:

1. **Hardcode pool names in application code** — leaks infrastructure topology into
   client SDKs and breaks when pools are renamed or reorganized
2. **Use a single pool** — sacrifices either isolation strength or resource efficiency
3. **Build custom routing** — every team reinvents the same webhook

This proposal standardizes option 3.

### Position in the AI agent stack

agent-sandbox sits in the middle of the cloud-native AI agent stack. Above it, agent
frameworks (LangChain, Claude Code, custom agents) create `SandboxClaim` resources.
Below it, runtimes provide isolation boundaries, and inference schedulers like llm-d
route LLM requests to vLLM workers based on KV-cache locality.

Our routing concern is orthogonal to inference routing: we route by **isolation tier**
(process vs. hardware boundary), while llm-d routes by **inference efficiency** (cache
hits, accelerator load). They compose at different layers without conflict.

Application-layer policy engines like NVIDIA OpenShell enforce filesystem, network, and
process restrictions inside the sandbox pod regardless of RuntimeClass. Our routing
selects which pool the pod comes from; OpenShell governs what the pod can do once
running. They stack independently.

## Design

### Annotations

Two annotations on `SandboxClaim.metadata.annotations`, using the existing
`agents.x-k8s.io/` prefix:

| Annotation | Values | Default | Description |
|------------|--------|---------|-------------|
| `agents.x-k8s.io/isolation` | `process`, `hardware` | *(unset — no routing)* | Isolation tier requested. When absent, the claim passes through unchanged. |
| `agents.x-k8s.io/overflow` | `allow`, `deny` | `allow` | Whether to fall back to another tier's pool when the preferred pool is exhausted |

**`isolation` values**:
- `process` — OS-level isolation (gVisor, runc). Sufficient for single-tenant
  environments and trusted agent workloads where kernel-level separation is adequate.
- `hardware` — Hardware-virtualized isolation (kata microVMs). Required for multi-tenant
  environments processing untrusted input, or when compliance mandates a dedicated
  guest kernel per sandbox.

**`overflow` semantics**:
- `allow` (default) — When the preferred tier's pool is exhausted, route to the other
  tier's pool. A gVisor sandbox in 0.32s is better than a cold kata start at 13s for
  workloads where hardware isolation is defense-in-depth but not a compliance
  requirement.
- `deny` — Never fall back across isolation tiers. Accept a cold start within the same
  tier rather than weaken the isolation guarantee. Use this when hardware isolation is
  mandated by policy (multi-tenant untrusted code execution, regulatory requirements).

### Pool mapping via ConfigMap

The webhook reads pool-to-tier mapping from a ConfigMap, not a CRD. Operators already
create pools manually; adding a CRD for routing policy is premature complexity.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sandbox-routing-config
  namespace: agent-sandbox-system
data:
  config.yaml: |
    tiers:
      process:
        pools:
          - name: pool-gvisor
          - name: pool-runc
      hardware:
        pools:
          - name: pool-kata
```

Pools are listed in preference order within each tier. The webhook tries them
sequentially.

### Webhook flow

```text
SandboxClaim CREATE
    │
    ▼
Webhook intercepts
    │
    ├── Has routing annotations? ──No──▶ Allow unchanged
    │
    ▼
Parse and validate isolation tier (required when routing)
    │
    ├── Unknown tier value? ──Yes──▶ Reject (admission error)
    │
    ▼
Look up tier's pool list from ConfigMap
    │
    ├── Tier missing or empty? ──Yes──▶ Reject (misconfiguration)
    │
    ▼
For each pool in preference order:
    │
    ├── pool.status.readyReplicas > 0? ──Yes──▶ Mutate warmPoolRef → pool.name
    │                                            Return patched claim
    ▼
All pools exhausted
    │
    ├── overflow=allow? ──Yes──▶ Try other tier's pools (same loop)
    │
    ├── overflow=deny?  ──Yes──▶ Route to first pool in own tier
    │                            (cold start within correct isolation)
    ▼
Return patched claim
```

Claims without routing annotations pass through unchanged — full backward
compatibility.

### Webhook implementation (simplified)

```go
var validTiers = map[string]bool{"process": true, "hardware": true}
var validOverflow = map[string]bool{"allow": true, "deny": true}

func (h *RoutingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
    claim := &extensionsv1beta1.SandboxClaim{}
    if err := h.Decoder.Decode(req, claim); err != nil {
        return admission.Errored(http.StatusBadRequest, err)
    }

    tier := claim.Annotations["agents.x-k8s.io/isolation"]
    if tier == "" {
        return admission.Allowed("no routing annotation")
    }
    if !validTiers[tier] {
        return admission.Denied(fmt.Sprintf("invalid isolation tier %q, must be process or hardware", tier))
    }

    overflow := claim.Annotations["agents.x-k8s.io/overflow"]
    if overflow == "" {
        overflow = "allow"
    }
    if !validOverflow[overflow] {
        return admission.Denied(fmt.Sprintf("invalid overflow value %q, must be allow or deny", overflow))
    }

    ns := req.Namespace
    pool, err := h.selectPool(ctx, tier, overflow, ns)
    if err != nil {
        return admission.Denied(err.Error())
    }

    mutated := claim.DeepCopy()
    mutated.Spec.WarmPoolRef.Name = pool
    return admission.PatchResponseFromRaw(req.Object.Raw, mutated)
}

func (h *RoutingWebhook) selectPool(ctx context.Context, tier, overflow, ns string) (string, error) {
    cfg := h.config.Load()

    tierCfg, ok := cfg.Tiers[tier]
    if !ok || len(tierCfg.Pools) == 0 {
        return "", fmt.Errorf("no pools configured for tier %q", tier)
    }

    if pool, found := h.firstHealthyPool(ctx, tierCfg.Pools, ns); found {
        return pool, nil
    }

    if overflow == "allow" {
        otherTier := "hardware"
        if tier == "hardware" {
            otherTier = "process"
        }
        if otherCfg, ok := cfg.Tiers[otherTier]; ok {
            if pool, found := h.firstHealthyPool(ctx, otherCfg.Pools, ns); found {
                return pool, nil
            }
        }
    }

    return tierCfg.Pools[0].Name, nil
}

func (h *RoutingWebhook) firstHealthyPool(ctx context.Context, pools []PoolRef, ns string) (string, bool) {
    for _, p := range pools {
        pool := &extensionsv1beta1.SandboxWarmPool{}
        if err := h.Client.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: ns}, pool); err != nil {
            continue
        }
        if pool.Status.ReadyReplicas > 0 {
            return p.Name, true
        }
    }
    return "", false
}
```

### MutatingWebhookConfiguration

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: sandbox-routing-webhook
webhooks:
  - name: routing.sandbox.agents.x-k8s.io
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      service:
        name: sandbox-routing-webhook
        namespace: agent-sandbox-system
        path: /mutate-sandboxclaim
    rules:
      - apiGroups: ["extensions.agents.x-k8s.io"]
        apiVersions: ["v1beta1"]
        resources: ["sandboxclaims"]
        operations: ["CREATE"]
    namespaceSelector:
      matchExpressions:
        - key: agents.x-k8s.io/routing-enabled
          operator: Exists
```

### RBAC

```yaml
rules:
  - apiGroups: ["extensions.agents.x-k8s.io"]
    resources: ["sandboxwarmpools"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get"]
    resourceNames: ["sandbox-routing-config"]
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["list", "watch"]
```

## Evidence

### Benchmark data (PR #1262)

Measured on GCP n2-standard-8 workers, 3-node cluster, pool sizes 4-24.

**Throughput ceiling** (burst recovery, pool=24, batch size=8):

| Runtime | Claims/sec | Limiting factor |
|---------|-----------|-----------------|
| runc    | 10.0      | Controller work-queue |
| gVisor  | 10.5      | Controller work-queue |
| kata    | 2.0       | VM boot + 250m CPU per slot |

**Quality zones** (warm claim latency distribution):

| Zone | Latency | Cause |
|------|---------|-------|
| Green | < 0.35s | Direct warm adoption |
| Grey  | 0.35-1.0s | Controller queue serialization |
| Cold  | > 1.0s | Pool exhausted, runtime cold start |

**Pool sizing guidance**:

| Runtime | Slots per CPU | Overhead per slot | Cold start penalty |
|---------|---------------|-------------------|--------------------|
| runc    | 1:1 | ~0 | None (indistinguishable from warm) |
| gVisor  | 1:1 | ~0 | Mild (P95 crosses 1s threshold) |
| kata    | 4:1 CPU, ~350Mi RAM | 250m CPU + 350Mi | Severe (13s P50) |

### Overflow decision examples

| Scenario | isolation | overflow | Behavior |
|----------|-----------|----------|----------|
| Interactive coding agent, single-tenant | `hardware` | `allow` | Prefer kata pool; fall back to gVisor if exhausted (0.32s vs 13s wait) |
| Multi-tenant untrusted code execution | `hardware` | `deny` | Kata pool only; accept cold start to maintain VM boundary |
| Low-risk batch data processing | `process` | `allow` | gVisor pool; fall back to runc if needed |
| Default (no annotations) | — | — | Existing warmPoolRef used unchanged |

## User experience

### Before (status quo)

```python
# Application must know pool names and infrastructure topology
if multi_tenant:
    pool = "pool-kata"
elif low_latency:
    pool = "pool-gvisor"
else:
    pool = "pool-runc"

claim = SandboxClaim(spec={"warmPoolRef": {"name": pool}})
```

### After (this proposal)

```python
# Application declares isolation intent
claim = SandboxClaim(
    metadata={"annotations": {
        "agents.x-k8s.io/isolation": "hardware",
        "agents.x-k8s.io/overflow": "allow",
    }},
    spec={"warmPoolRef": {"name": "pool-default"}},
)
```

The webhook rewrites `warmPoolRef` before the claim reaches the controller.

## Alternatives considered

### 1. Client-side routing library

Provide an SDK that queries pool status and selects the optimal pool before creating
the claim. Rejected: requires SDK changes in every language, introduces race conditions
between query and submission, duplicates routing logic across clients, and does not help
kubectl/YAML workflows.

### 2. Pool label selectors

Add label selectors to `SandboxWarmPool` that match claim labels, similar to Services.
Rejected: does not support ordered preferences or fallback across tiers, harder to
implement graceful degradation, and requires significant controller changes.

### 3. External router service

Deploy a separate router pod that clients query via HTTP API before claim creation.
Rejected: not Kubernetes-native, requires client changes, introduces an additional
failure mode, and has no integration with kubectl workflows.

## Security considerations

1. **Webhook availability**: `failurePolicy: Fail` blocks claim creation when the
   webhook is unavailable. This is deliberate — with `Ignore`, a `hardware`/`deny`
   claim would pass through with its original placeholder `warmPoolRef`, silently
   violating the requested isolation guarantee. Operators who prefer availability
   over isolation enforcement can change to `Ignore`, understanding that webhook
   downtime degrades routing to best-effort.

2. **CREATE-only operations**: The webhook intercepts only `CREATE`, not `UPDATE`.
   This is correct because `spec.warmPoolRef` is effectively immutable after the
   claim controller adopts a sandbox — changing it post-adoption has no effect.
   Annotation changes on existing claims do not re-route.

3. **Annotation abuse**: Routing annotations are **advisory, not an authorization
   boundary**. Any user who can create a `SandboxClaim` in a routing-enabled
   namespace can request `isolation: hardware` and consume kata pool capacity.
   Mitigations: `namespaceSelector` limits which namespaces participate in routing,
   and pool capacity is bounded by `Replicas`. For stricter enforcement, operators
   can deploy a `ValidatingAdmissionPolicy` that restricts which namespaces or
   subjects may use specific tier values. Per-tier quotas are deferred to future work.

4. **Pool exhaustion**: Rapid claims can exhaust all pools regardless of routing.
   Kubernetes native ResourceQuota and LimitRange apply. Future work may add
   per-tier rate limits.

## Observability

Webhook emits Prometheus metrics:

- `sandbox_routing_decisions_total{tier, pool, overflow}` — routing decision counter
- `sandbox_routing_duration_seconds` — webhook latency histogram
- `sandbox_routing_fallbacks_total{from_tier, to_tier}` — overflow event counter
- `sandbox_routing_pool_exhausted_total{tier}` — pool exhaustion counter

## Testing plan

Extend the existing benchmark framework from PR #1262:

1. **Unit tests**: annotation parsing, pool selection logic, overflow behavior,
   ConfigMap parsing
2. **Integration tests**: end-to-end claim routing with multiple pools, pool
   exhaustion triggering overflow, webhook failure passthrough
3. **Benchmark extension**: measure routing overhead (target: <10ms added latency),
   verify warm claim latency is unchanged after routing

## Future work (explicitly deferred)

- **CRD-based routing policy**: Replace ConfigMap with a `SandboxRoutingPolicy` CRD
  when the routing model stabilizes and operators need status reporting
- **Autoprovisioning**: Controller that creates pools referenced in routing config
  if they don't exist
- **Confidential Containers (Ring 3)**: A third isolation tier for silicon-level
  memory protection, extending the `isolation` annotation with a `confidential` value
- **llm-d integration**: End-to-end latency awareness combining sandbox claim routing
  with inference request routing for holistic SLA management
- **Per-tier autoscaling**: HPA-style scaling of pool replicas based on claim rate
  and overflow frequency

## References

- [Kubernetes Admission Webhooks](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)
- [PR #1262: Runtime class benchmarks](https://github.com/kubernetes-sigs/agent-sandbox/pull/1262)
- [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) — application-layer policy enforcement for agent sandboxes
- [llm-d](https://github.com/llm-d/llm-d) — CNCF Sandbox project for cache-aware LLM inference routing
