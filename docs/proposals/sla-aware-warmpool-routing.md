# Proposal: Isolation-Tier Routing for SandboxClaims

**Status**: Draft Proposal
**Authors**: @vvoronko
**Created**: 2026-07-14
**Updated**: 2026-07-25

## Summary

This proposal introduces an optional mutating admission webhook that routes
`SandboxClaim` resources to the appropriate `SandboxWarmPool` based on isolation-tier
spec fields. Instead of clients hardcoding a pool name, they declare the isolation
level they need (`process` or `hardware`) and whether fallback across tiers is
acceptable. The webhook rewrites `spec.warmPoolRef.name` at admission time. The
existing controller logic is unchanged; the only API-surface addition is two optional
pointer fields on `SandboxClaimSpec` (see [Spec fields](#spec-fields)).

This is a **Kubernetes-native routing option** (Tier 3) for operators running multi-pool
clusters without an upper-layer orchestrator like Agent Substrate. Deployments using
Substrate or similar data-plane orchestrators resolve pool binding in their own fast
path and are unaffected by this proposal.

## Motivation

### Warm claims are runtime-independent

Benchmark data collected using the test harness from PR #1262 (3 runtimes, 6 pool
sizes, n2-standard-8 GCP workers) shows that **warm claim latency is identical across
runtimes**:

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

agent-sandbox sits in the middle of the cloud-native AI agent stack:

```text
Agent frameworks (LangChain, Claude Code, custom agents)
    ↓ SandboxClaim
┌── Agent Substrate (optional) ──────────────────────────┐
│   Actor packing, data-plane routing (atenet/Envoy),    │
│   30x+ oversubscription, bypasses K8s hot path         │
└── sets warmPoolRef directly ───────────────────────────┘
    ↓ SandboxClaim (warmPoolRef resolved)
agent-sandbox controller (warm pools, pod lifecycle)     ← THIS PROJECT
    ↓ Pod with RuntimeClass
OpenShell (seccomp, Landlock, eBPF policy enforcement)   ← app-layer, orthogonal
    ↓ kernel boundary
gVisor / Kata microVM / runc                             ← isolation runtime
    ↓ inference calls
llm-d (KV-cache-aware routing, prefill/decode split)     ← inference plane
    ↓
vLLM workers (GPU inference engine)
```

[Agent Substrate](https://github.com/agent-substrate/substrate) is a Google-led project
that packs many actors onto fewer worker pods, achieving 30x+ oversubscription. It takes
agent-sandbox's secure runtime and snapshotting capabilities and pairs them with a
minimal control plane designed for ultra-scale. Substrate resolves pool binding in its
own data plane and sets `warmPoolRef` directly — it does not need a Kubernetes webhook
for routing.

### Routing tiers

Pool routing can happen at three levels, depending on the deployment's complexity:

| Tier | Mechanism | When to use |
|------|-----------|-------------|
| **1. Explicit** | Client sets `spec.warmPoolRef` directly | Single-pool clusters, static environments |
| **2. Data-plane** | Substrate (or similar orchestrator) resolves pool binding in its own fast path, sets `warmPoolRef` before submitting the claim | High-density, ultra-scale deployments with an L7 orchestrator |
| **3. Control-plane** | Mutating webhook rewrites `warmPoolRef` at admission time based on `spec.isolation` | Multi-pool Kubernetes clusters without an upper-layer orchestrator |

**This proposal implements Tier 3** — the Kubernetes-native option for operators who
run mixed isolation environments (gVisor + kata pools) without Substrate or a custom
orchestrator. Tier 1 and Tier 2 are unaffected: claims without `spec.isolation` pass
through unchanged, and Substrate can continue setting `warmPoolRef` directly.

The tiers are not mutually exclusive. An operator can run Substrate for high-density
namespaces (Tier 2) while using the webhook for vanilla namespaces (Tier 3), with
`namespaceSelector` controlling which namespaces opt into webhook routing.

### Orthogonal concerns

- **Inference routing**: llm-d routes by KV-cache locality and GPU load within the
  inference plane. We route by isolation tier in the sandbox control plane. They compose
  at different layers without conflict.
- **Application-layer policy**: NVIDIA OpenShell enforces filesystem, network, and
  process restrictions inside the sandbox pod regardless of RuntimeClass. Our routing
  selects which pool the pod comes from; OpenShell governs what the pod can do once
  running. They stack independently.

## Design

### Spec fields

Two optional fields on `SandboxClaim.spec`:

| Field | Type | Values | Default | Description |
|-------|------|--------|---------|-------------|
| `spec.isolation` | `*IsolationTier` (enum) | `process`, `hardware` | *(nil — no routing)* | Isolation tier requested. When nil, the claim passes through unchanged. |
| `spec.overflow` | `*OverflowPolicy` (enum) | `allow`, `deny` | `allow` | Whether to fall back to a lower-isolation tier's pool when the preferred pool is exhausted. Ignored when `spec.isolation` is nil (no routing). |

Both fields are optional and nil by default, so existing claims without them behave
exactly as today — full backward compatibility. Because they are typed spec fields,
values get OpenAPI enum validation (`+kubebuilder:validation:Enum`), appear in
`kubectl explain sandboxclaim.spec`, and follow the standard Kubernetes
versioning/deprecation lifecycle.

**API changes required**: The following additions to `SandboxClaimSpec` in
`extensions/api/v1beta1/sandboxclaim_types.go` are non-breaking — optional pointer
fields with `omitempty` leave existing claims unchanged:

```go
// IsolationTier specifies the isolation boundary for the sandbox.
// +kubebuilder:validation:Enum=process;hardware
type IsolationTier string

const (
    IsolationTierProcess  IsolationTier = "process"
    IsolationTierHardware IsolationTier = "hardware"
)

// OverflowPolicy specifies whether a claim may fall back to a lower-isolation tier.
// +kubebuilder:validation:Enum=allow;deny
type OverflowPolicy string

const (
    OverflowPolicyAllow OverflowPolicy = "allow"
    OverflowPolicyDeny  OverflowPolicy = "deny"
)
```

Added to `SandboxClaimSpec`:

```go
// isolation selects the isolation tier for pool routing.
// When nil, the claim is not routed and warmPoolRef is used as-is.
// +optional
Isolation *IsolationTier `json:"isolation,omitempty"`

// overflow controls whether the claim may fall back to a lower-isolation
// tier's pool when the preferred tier is exhausted.
// Defaults to "allow" when isolation is set.
// +optional
Overflow *OverflowPolicy `json:"overflow,omitempty"`
```

The v1alpha1 conversion webhook (`extensions/api/v1alpha1/sandboxclaim_conversion.go`)
must be updated to round-trip these fields. Run `make manifests` to regenerate CRD
schemas after modifying the types.

**`isolation` values**:
- `process` — OS-level isolation (gVisor, runc). Sufficient for single-tenant
  environments and trusted agent workloads where kernel-level separation is adequate.
- `hardware` — Hardware-virtualized isolation (kata microVMs). Required for multi-tenant
  environments processing untrusted input, or when compliance mandates a dedicated
  guest kernel per sandbox.

**`overflow` semantics** (tier-down only):
- `allow` (default) — When the preferred tier's pool is exhausted, fall back to a
  lower-isolation tier. In practice this means `hardware` claims can overflow to
  `process` pools (a gVisor sandbox in 0.32s is better than a cold kata start at 13s).
  The reverse (`process` → `hardware`) is never attempted — it would spend scarce
  kata capacity (250m CPU + 350Mi per slot) on workloads that did not request hardware
  isolation, starving future `hardware`/`deny` claims. Process-tier pools already
  have within-tier fallback (gVisor → runc) at no extra cost.
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

**Namespace-scoped resolution**: Pool names in the ConfigMap are resolved within the
claim's namespace (`req.Namespace`). Every routing-enabled namespace must provision
pools with the names listed in the ConfigMap. If a required pool does not exist in the
claim's namespace, the webhook rejects the claim with an explicit error naming the
missing pool — no silent fallthrough, no degraded routing. This enforces a consistent
naming convention across namespaces and makes misconfigurations immediately visible.

### Webhook flow

```text
SandboxClaim CREATE
    │
    ▼
Webhook intercepts
    │
    ├── spec.isolation nil? ──Yes──▶ Allow unchanged
    │
    ▼
Look up tier's pool list from ConfigMap
    │
    ├── Tier missing or empty? ──Yes──▶ Reject (misconfiguration)
    │
    ▼
For each pool in preference order:
    │
    ├── Pool missing in namespace? ──Yes──▶ Reject (naming convention violated)
    │
    ├── pool.status.readyReplicas > 0? ──Yes──▶ Mutate warmPoolRef → pool.name
    │                                            Return patched claim
    ▼
All pools exhausted (exist but empty)
    │
    ├── overflow=allow AND tier=hardware?
    │       ──Yes──▶ Try process tier's pools (tier-down fallback)
    │
    ├── Otherwise ──────▶ Route to first pool in own tier
    │                            (cold start within correct isolation)
    ▼
Return patched claim
```

Claims without routing fields pass through unchanged — full backward
compatibility.

### Webhook implementation (simplified)

```go
func (h *RoutingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
    claim := &extensionsv1beta1.SandboxClaim{}
    if err := h.Decoder.Decode(req, claim); err != nil {
        return admission.Errored(http.StatusBadRequest, err)
    }

    // overflow without isolation is a no-op: routing only activates when the
    // caller explicitly requests an isolation tier.
    if claim.Spec.Isolation == nil {
        return admission.Allowed("no isolation tier requested — overflow ignored if set")
    }
    tier := string(*claim.Spec.Isolation)

    overflow := "allow"
    if claim.Spec.Overflow != nil {
        overflow = string(*claim.Spec.Overflow)
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

    pool, err := h.firstHealthyPool(ctx, tierCfg.Pools, ns)
    if err != nil {
        return "", err
    }
    if pool != "" {
        return pool, nil
    }

    // Overflow is tier-down only: hardware → process.
    // process → hardware is never attempted to avoid starving
    // scarce kata capacity with workloads that don't need it.
    if overflow == "allow" && tier == "hardware" {
        if processCfg, ok := cfg.Tiers["process"]; ok {
            pool, err := h.firstHealthyPool(ctx, processCfg.Pools, ns)
            if err != nil {
                return "", err
            }
            if pool != "" {
                return pool, nil
            }
        }
    }

    return tierCfg.Pools[0].Name, nil
}

func (h *RoutingWebhook) firstHealthyPool(ctx context.Context, pools []PoolRef, ns string) (string, error) {
    selected := ""
    for _, p := range pools {
        pool := &extensionsv1beta1.SandboxWarmPool{}
        if err := h.Client.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: ns}, pool); err != nil {
            if apierrors.IsNotFound(err) {
                return "", fmt.Errorf("pool %q not found in namespace %q — routing requires all configured pools to exist", p.Name, ns)
            }
            return "", fmt.Errorf("failed to check pool %q in namespace %q: %w", p.Name, ns, err)
        }
        if selected == "" && pool.Status.ReadyReplicas > 0 {
            selected = p.Name
        }
    }
    return selected, nil
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
    matchConditions:
      - name: has-isolation
        expression: "has(object.spec.isolation)"
    namespaceSelector:
      matchExpressions:
        - key: agents.x-k8s.io/routing-enabled
          operator: Exists
```

### RBAC

ClusterRole for cross-namespace pool lookups:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: sandbox-routing-webhook
rules:
  - apiGroups: ["extensions.agents.x-k8s.io"]
    resources: ["sandboxwarmpools"]
    verbs: ["get", "list", "watch"]
```

Namespace-scoped Role for the routing ConfigMap (webhook namespace only):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sandbox-routing-webhook-config
  namespace: agent-sandbox-system
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "watch"]
    resourceNames: ["sandbox-routing-config"]
```

## Evidence

### Benchmark data

Measured using the test harness from PR #1262 on GCP n2-standard-8 workers, 3-node
cluster, pool sizes 4-24. PR #1262 provides the harness and CSV report generation;
the numbers below are from a representative run.

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
| Interactive coding agent, single-tenant | `hardware` | `allow` | Prefer kata pool; fall back to gVisor/runc if exhausted (0.32s vs 13s wait) |
| Multi-tenant untrusted code execution | `hardware` | `deny` | Kata pool only; accept cold start to maintain VM boundary |
| Low-risk batch data processing | `process` | `allow` | gVisor pool; within-tier fallback to runc (never overflows to kata) |
| Default (fields unset) | — | — | Existing warmPoolRef used unchanged |

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
# Application declares isolation intent; warmPoolRef is required by the CRD
# but the webhook overwrites it with the selected pool before admission
claim = SandboxClaim(
    spec={
        "isolation": "hardware",
        "overflow": "allow",
        "warmPoolRef": {"name": "placeholder"},
    },
)
```

The webhook rewrites `warmPoolRef.name` before the claim reaches the controller.
The placeholder value is never used.

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
   violating the requested isolation guarantee. The `matchConditions` CEL filter
   (`has(object.spec.isolation)`) narrows the scope: only claims that set
   `spec.isolation` are sent to the webhook. Claims without routing fields bypass
   the webhook entirely, even during outages — so webhook downtime never affects
   non-routed claims. Operators who prefer availability over isolation enforcement
   can change to `Ignore`, understanding that webhook downtime degrades routing to
   best-effort.

2. **CREATE-only operations**: The webhook intercepts only `CREATE`, not `UPDATE`.
   This is correct because `spec.warmPoolRef` is effectively immutable after the
   claim controller adopts a sandbox — changing it post-adoption has no effect.
   Changes to `spec.isolation` on existing claims do not re-route.

3. **Tier field abuse**: Routing fields are **advisory, not an authorization
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

1. **Unit tests**: spec field handling, pool selection logic, overflow behavior,
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
  memory protection, extending `spec.isolation` with a `confidential` value
- **llm-d integration**: End-to-end latency awareness combining sandbox claim routing
  with inference request routing for holistic SLA management
- **Per-tier autoscaling**: HPA-style scaling of pool replicas based on claim rate
  and overflow frequency

## References

- [Kubernetes Admission Webhooks](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)
- [PR #1262: Runtime class benchmark harness](https://github.com/kubernetes-sigs/agent-sandbox/pull/1262) — test framework and CSV report generation for the data cited above
- [Agent Substrate](https://github.com/agent-substrate/substrate) — data-plane actor packing and routing for agent-sandbox at ultra-scale
- [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) — application-layer policy enforcement for agent sandboxes
- [llm-d](https://github.com/llm-d/llm-d) — CNCF Sandbox project for cache-aware LLM inference routing
