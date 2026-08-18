# Architecture

## Design goals

1. **Scale ceiling** — reach 1-3M sandboxes across ~20-60 clusters (target
   scale), from today's ~50k/cluster ceiling. See the capacity-cliff analysis
   in `dev/load-test/test-recipes/README-capacity-cliff.md`.
2. **No k8s federation stack** — every published RL platform (Cognition,
   Anthropic, Prime Intellect, Alibaba) has converged on per-cluster agent +
   object-storage coordination. We match the industry pattern.
3. **Durable state** — the driver must be able to crash and recover. The RL
   PoC (`examples/agent-sandbox-rl/`) keeps state in-process — a killed driver
   orphans placement bookkeeping and requires a label-scoped sweep to recover.
4. **Capacity-aware placement** — production workloads cannot use static
   weights; placement must respond to live cluster pressure.
5. **Weight/artifact distribution** — the missing piece from the RL PoC. Model
   directly on Cognition's tree-broadcast pattern (SWE-1.7 writeup, July 2026).

## Control-plane shape

There is **no hub controller** and **no hub apiserver**. The coordination
substrate is a GCS bucket. Every component is either a data-plane agent or a
CLI/batch job.

```
                       ┌──────────────────────────────┐
                       │       GCS bucket (hub)       │
                       │                              │
                       │  fleet/spec.json             │
                       │  fleet/assignments.json      │
                       │  fleet/capacity/<cluster>.json│
                       │  weights/manifest.json       │
                       │  weights/deltas/v<N>.bin     │
                       └───▲───────────▲──────────▲───┘
                           │           │          │
     ┌─────────────────────┘           │          └───────────────────┐
     │ writes spec+assignments,        │ writes deltas+manifest       │
     │ reads capacity                  │                              │
     ▼                                 ▼                              ▼
 ┌─────────┐                    ┌──────────────┐              ┌──────────────┐
 │ planner │                    │  trainer     │              │  agent (Go)  │
 │ (Python │                    │  (Python or  │              │  per cluster │
 │ CLI)    │                    │  real)       │              │              │
 └─────────┘                    └──────────────┘              │ ┌──────────┐ │
                                                              │ │ fleet    │ │
                                                              │ │ reconc.  │ │
                                                              │ ├──────────┤ │
                                                              │ │ capacity │ │
                                                              │ │ reporter │ │
                                                              │ ├──────────┤ │
                                                              │ │ weight   │ │
                                                              │ │ sync     │ │
                                                              │ └──────────┘ │
                                                              └──────┬───────┘
                                                                     │
                                                                     ▼
                                                              ┌──────────────┐
                                                              │ local cluster│
                                                              │ Sandbox CRs, │
                                                              │ SandboxTmpl, │
                                                              │ SandboxWarm- │
                                                              │ Pool         │
                                                              └──────────────┘
```

## Data flow

### Placement cycle (every planner apply, or every 60s if a controller loop is added later)

1. **Planner** reads `fleet/spec.json` (or accepts `-f` on CLI).
2. **Planner** lists `fleet/capacity/` to build a `ClusterRegistry` of live
   clusters with fresh capacity signals.
3. **Planner** runs harvested `placement.CapacityAware` over the registry:
   - Uses image-affinity (MD5 mod #clusters) to keep same-image tasks on the
     same cluster (image-pull locality — the RL PoC's original insight, worth
     keeping).
   - Falls back to LeastLoaded (min active_claims, active_replicas) if the
     affinity target is at capacity.
   - New: filters out clusters whose capacity report is >90s stale.
4. **Planner** runs harvested `budget.hamilton_split` to divide
   `spec.max_concurrent` across placed clusters by weight.
5. **Planner** runs harvested `sizing.compute_replicas` per (cluster, image)
   pair to size the warm pool.
6. **Planner** writes `fleet/assignments.json`:
   ```json
   {
     "generation": 42,
     "updated_at": "2026-07-08T15:34:12Z",
     "clusters": {
       "fleet-a": {
         "pools": [
           {"image": "gcr.io/.../swebench-django:v1", "template": "django-tmpl", "warmpool": "django-pool", "replicas": 15},
           ...
         ]
       },
       "fleet-b": { "pools": [...] }
     }
   }
   ```

### Agent reconcile loop (per cluster, every 30s + watch on assignments.json etag)

1. **Fleet reconciler** polls `fleet/assignments.json` (etag-conditional GET).
   On change, extracts the entry for its own cluster (`--cluster-name` flag).
2. For each `pool` in the assignment:
   - Ensures a `SandboxTemplate` exists (creates from `spec.templates[image]`
     if missing; updates image if drifted).
   - Ensures a `SandboxWarmPool` exists (creates if missing; patches
     `spec.replicas` if drifted).
3. **Removes** SandboxWarmPools with label `fleet.agent-sandbox.io/managed=true`
   that are not in the current assignment.
4. **Capacity reporter** every 30 s:
   - Lists all `SandboxWarmPool` in the cluster with the fleet-managed label.
   - Sums `Status.Replicas` and `Status.ReadyReplicas` → `warmpool_depth`.
   - Queries the local `agent_sandbox_claim_startup_latency_ms` metric via
     the sandbox controller's `/metrics` endpoint → `claim_p90_ms`.
   - Lists nodes, computes crude `node_pressure_score` (allocatable-vs-used
     CPU/memory ratio, averaged).
   - Writes `fleet/capacity/<cluster>.json`.

### Weight sync loop (per cluster, every 5s + watch on manifest etag)

1. **Weight sync** polls `weights/manifest.json`.
2. On version change:
   - Downloads `weights/deltas/v<current>.bin` into a local staging dir
     (backing a `PersistentVolume` on each node — `hostPath` in kind).
   - Once the file is fully staged, patches
     `fleet.agent-sandbox.io/weight-version=<N>` on every pod owned by a
     fleet-managed SandboxWarmPool.
   - Pods (mock inference sandbox) watch that label and reload — the mock
     just exposes `/version` returning the current version.
   - In-flight requests keep serving on the old version until the pod pulls
     the new one (matching Cognition's "trajectories continue on new weights
     with KV cache intact" pattern).

## Wire format (the hub contract)

The GCS layout is the ONLY contract between planner, agents, and trainer.
It's documented once here, and enforced via matched schemas in Go
(`pkg/fleet/types.go`) and Python (`agent_sandbox_fleet/objectstore.py`).

### `fleet/spec.json` (written by planner or CLI apply)

```json
{
  "generation": 42,
  "max_concurrent": 100,
  "placement_policy": "capacity-aware",
  "min_clusters": 5,
  "cluster_weights": {"fleet-a": 1.0, "fleet-b": 1.0},
  "models": [
    {
      "image": "gcr.io/.../swebench-django:v1",
      "template_name": "django-tmpl",
      "target_tasks": 40,
      "weight_stream": "swebench-actor-v1"
    },
    ...
  ]
}
```

`min_clusters` (optional, default 0): anti-affinity floor. When set, ALL
models placed round-robin across the first `min(min_clusters, len(fresh))`
sorted-by-name fresh clusters, ignoring `placement_policy`. Deterministic —
same spec + same fresh set produces byte-identical placement, so re-apply
cannot ping-pong. Use for `models > clusters` cases where the default
spread-first + scored-extras path could oscillate.

### `fleet/assignments.json` (written by planner)

Structure above under "Placement cycle" step 6.

### `fleet/capacity/<cluster>.json` (written by agent every 30s)

```json
{
  "cluster": "fleet-a",
  "updated_at": "2026-07-08T15:34:12Z",
  "generation_observed": 42,
  "warmpool_depth": 15,
  "warmpool_ready": 14,
  "active_claims": 3,
  "claim_p90_ms": 220.5,
  "node_pressure_score": 0.42,
  "reported_pools": ["django-pool", "sympy-pool"]
}
```

### `weights/manifest.json` (written by trainer)

```json
{
  "current_version": 5,
  "previous_version": 4,
  "delta_path": "weights/deltas/v5.bin",
  "delta_size_bytes": 1048576,
  "weight_stream": "swebench-actor-v1",
  "published_at": "2026-07-08T15:35:00Z"
}
```

`current_version` monotonically increases. Agents track their observed version
per stream. Multiple streams (e.g. different actor classes) live under
`weights/streams/<stream>/manifest.json` — this PoC ships one stream only.

## Failure modes and recovery

| Failure | Behavior | Recovery |
|---|---|---|
| Agent crashes | Its capacity report goes stale (>90s). Next planner apply excludes the cluster. Local warmpools continue to serve. | Restart the agent. On next reconcile it publishes fresh capacity and gets included on next apply. |
| GCS unreachable from an agent | Reconcile loop backs off (exponential to 60s). Warmpools continue serving whatever they last knew. | Auto-recovers when GCS returns. |
| Assignments file corrupt | Agent logs the parse error, keeps last-known-good in memory, capacity reports still flow. | Planner writes a fresh assignments.json. |
| Planner run with a cluster missing capacity report | Excluded from placement. Warmpools on that cluster orphan (kept alive by their own agent, but no new assignments come in). | Cluster agent republishes capacity → next apply includes it → assignments trickle in. |
| Weight delta upload half-fails | Agents see the new manifest but partial delta; download fails with checksum mismatch (mock adds SHA256). Retry with backoff. | Trainer republishes. |
| Weight-sync patches a label on a pod that's mid-request | The mock inference sandbox reads the label on next request; existing requests complete on old version. Matches Cognition's KV-cache-intact model. | N/A — by design. |

## Why NOT the alternatives (short version)

| Rejected option | Reason |
|---|---|
| Karmada / KubeFleet / OCM | Full extra control plane; per-CRD Lua interpreters (Karmada); adds a scheduler layer we don't need; no published RL platform uses one. |
| Kueue MultiKueue | Wrong shape — batch queue semantics, quota-gated placement (not capacity-aware), arbitrary CRDs need compiled-in Go adapters (KEP-693). |
| KubeFed | Archived April 2023. |
| Liqo / Admiralty / Virtual Kubelet | Operate at Pod level; would strip Sandbox CR identity, PVCs, and status from the origin cluster. |
| GKE MCS + Multi-Cluster Gateway | Solves *traffic routing*, not placement or artifact distribution. Complementary — a real system would use MCS for a *separate* general-claim federation track. |
| A hub controller with a hub apiserver | Adds a hub-cluster failure mode. Object storage is more durable, cheaper, and matches the industry pattern. |


## Substrate compatibility

**Substrate** (github.com/agent-substrate/substrate) is a peer OSS project.
Not a layer above or below agent-sandbox — an independent Google-adjacent
system for stateful "actor" workloads on Kubernetes. Both projects need
multi-cluster support. This fleet is designed to serve both from one binary.

### Shape mapping

| agent-sandbox (extensions.agents.x-k8s.io/v1beta1) | substrate (substrate.ate.dev/v1alpha1) | Reconcile? |
|---|---|---|
| `SandboxWarmPool` (replicas, template, HPA) | `WorkerPool` (replicas, template, `/scale`) | **Same code path** — namespaced, template-referenced, scalar replicas |
| `SandboxTemplate` (blueprint) | `ActorTemplate` (immutable-spec CEL) | **Same code path** — namespaced, referenced by pool |
| `Sandbox` (per-instance CR) | Actor (**Redis record via `ate-api-server` gRPC**) | **Fundamentally different** — substrate has no per-instance CR |
| (none) | `SandboxConfig` (cluster-scoped, runsc/kata binary URLs) | v3 — needs cluster-scoped propagation |

Substrate's whole design deliberately keeps per-instance state out of etcd
(actors live in Redis) to avoid the CRD scale cliff sandbox is currently
hitting. That's actually helpful here: **the shared surface is the
warm-pool + template layer, which is workload-agnostic**. The per-instance
layer that diverges (Sandbox CRs vs. Actor gRPC) is not something this
fleet controller places anyway — placement decisions land on pools, not
instances.

### The seam: a `WorkloadKind` interface

None of this is implemented — the fleet-member reconciles `SandboxWarmPool`
directly. It is recorded here as the shape a second workload kind would take,
so the wire format above stays honest about what it is not yet abstracting.

A small interface would carry it:

```go
type WorkloadKind interface {
    Name() string
    PoolGVR() schema.GroupVersionResource
    TemplateGVR() schema.GroupVersionResource
    BuildTemplate(p AssignmentPool, namespace string) *unstructured.Unstructured
    BuildPool(p AssignmentPool, namespace string) *unstructured.Unstructured
    ReadPoolStatus(u *unstructured.Unstructured) (depth, ready int)
}
```

Two impls would exist:

- **`SandboxKind`** — reconciles `SandboxWarmPool` + `SandboxTemplate`, reads
  `.status.replicas` + `.status.readyReplicas`. This is the behaviour the
  Python fleet-member implements inline today.
- **`SubstrateKind`** — reconciles `WorkerPool` + `ActorTemplate`. Status shape
  may differ slightly; the interface method lets substrate override.

The reconcile and capacity-report loops would route through the interface, and
the member would select a kind at startup. Worth doing only when someone has a
substrate cluster to point it at.

### What stays shared vs. what diverges

**Shared** (workload-agnostic, ~90% of the code):
- Object-storage hub pattern + wire schemas (`FleetSpec`, `Assignments`,
  `CapacityReport`, `WeightManifest`)
- Placement algorithms (`ImageAffinity`, `CapacityAware`, Hamilton budget
  split)
- Per-cluster agent skeleton + the 3 reconcile loops
- `fleetctl` CLI + Python planner
- Weight/artifact distribution pattern (v2)

**Diverges** (per-kind plugin):
- Which CRD group/version/resource to reconcile
- How to build the template + pool spec for a new placement
- How to read pool status
- (v3) how to fetch capacity signals — substrate may pull from Redis /
  `ate-api-server` instead of pool `.status`

**Explicitly NOT shared** (each project owns its own thing):
- Per-instance placement logic (sandbox has Sandbox CRs; substrate has
  actor records in Redis behind gRPC)
- Scale-cliff analysis (sandbox = kube-apiserver; substrate = Redis
  throughput)
- Any assumption that "list instances" == "kubectl list CR"

### v3 ship gate

The substrate impl only makes sense to ship when someone actually has a
substrate cluster to point it at. Until then, the interface exists as a
concrete artifact — not a hook for hypothetical future workloads. Do not
generalize the interface for a third workload family without a real use
case.

## Trust boundaries

- **Planner** needs GCS read+write on `fleet/`. No k8s access.
- **Trainer** needs GCS read+write on `weights/`. No k8s access.
- **Agent** needs:
  - GCS read on `fleet/spec.json` + `fleet/assignments.json` + `weights/**`.
  - GCS write on `fleet/capacity/<own-cluster>.json`.
  - K8s RBAC for `sandboxes`, `sandboxtemplates`, `sandboxwarmpools`
    (get/list/watch/create/update/patch/delete), `pods` (get/list/patch),
    `nodes` (get/list).

In the PoC, GCS access is via a service-account JSON key mounted from a
Secret. In production, use Workload Identity (GKE) or IRSA (EKS) — no key on
disk.

## Metrics (out of PoC scope, sketched)

Every agent would expose:

- `fleet_reconcile_total{result="ok|err"}`
- `fleet_capacity_report_age_seconds`
- `fleet_warmpool_managed_count`
- `fleet_weight_sync_lag_seconds{stream}`
- `fleet_weight_version{stream}` (gauge)
- `fleet_gcs_op_duration_seconds{op}`

A fleet-wide Grafana dashboard would join these across clusters via a Prometheus
federation setup or a single VictoriaMetrics cluster.
