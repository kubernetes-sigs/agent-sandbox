# KEP-1434: Multi-Cluster Fleet for Batch and RL Sandbox Workloads

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
  - [High-Level Design](#high-level-design)
    - [Components](#components)
    - [Coordination Substrate](#coordination-substrate)
    - [Placement Algorithm](#placement-algorithm)
    - [Failure Handling](#failure-handling)
    - [Example FleetSpec](#example-fleetspec)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
- [Scalability](#scalability)
- [Alternatives (Optional)](#alternatives-optional)
<!-- /toc -->

## Summary

Reinforcement-learning and batch workloads need 100K–1M concurrent sandboxes, which is more than any single Kubernetes cluster holds. This KEP proposes a **fleet layer** that spreads `SandboxWarmPool` objects across N clusters from one declarative spec: a single-replica `fleet-member` Deployment on each cluster, plus a stateless `fleetctl` planner CLI that an admin runs from outside the fleet. The two coordinate through one object-storage bucket.

There is no hub apiserver, no shared etcd, no cross-cluster networking, and no new CRD. The fleet layer only decides *which cluster gets which warm pool*; the existing `SandboxWarmPool` and `SandboxTemplate` controllers do all the actual work. It ships as a reference example and Python library under [`fleet/`](../../../fleet/), not as part of the core controller.

The design is not hypothetical — it has run at **1,069,000 concurrent sandboxes across six clusters** from a single `fleetctl apply`, reproduced twice. See [Scalability](#scalability).

## Motivation

A single cluster has a measured ceiling. At `max-pods 256` on 8-vCPU nodes we sustain roughly **236 sandbox pods per node**, so a 999-node cluster holds about 234,500 sandboxes and an 850-node cluster about 199,000. A single cluster therefore tops out in the low 200Ks under any tuning we have found. No amount of controller or node configuration reaches 1M in one cluster.

Users who need that scale are already solving this by hand: standing up N clusters, hand-partitioning their workload across them, and running one `kubectl apply` per cluster with per-cluster replica counts they computed in a spreadsheet. That approach is error-prone (the partition is recomputed by hand every time cluster sizes change), has no failure story (a degraded cluster keeps receiving work until a human notices), and produces no fleet-wide view.

Two workload shapes motivate this:

1. **RL / batch.** High image cardinality and high task count. Work can be pre-planned, images pre-pulled, and clusters prepared before the job starts. This is the primary target.
2. **Sandbox production serving.** Needed once production usage exceeds what one cluster holds while still meeting latency expectations. Secondary — see [Non-Goals](#non-goals).

### Goals

- Place warm pools across N clusters from one declarative spec and one command.
- **Derive** per-cluster budgets and per-pool replica counts from live capacity rather than having an admin author them.
- Automatically exclude a degraded or unreachable cluster from the next placement pass.
- Keep placement deterministic, so re-applying an unchanged spec does not churn pools.
- Add **no new CRDs, no new controller, and no hub apiserver**.
- Ship as an example plus a reusable Python library, so an external scheduler can import the planner without adopting the daemon.

### Non-Goals

- **Not a general multi-cluster control plane.** This does not schedule arbitrary workloads, federate arbitrary resources, or replace Karmada/KubeFleet/OCM.
- **Not interactive serving.** Batch and RL only. Interactive code execution would need queue-depth observability and a much tighter reconcile loop than a 30-second poll.
- **Not an autoscaler.** Scaling is an admin re-applying an edited spec. There is no closed loop from load to capacity.
- **Not cross-cluster networking.** No multi-cluster Services, DNS, or service discovery. Each sandbox is reached through its own cluster.
- **Not a throughput improvement.** The fleet layer costs about 2.5 seconds per apply; everything after that is the same upstream controller work. Its value is one command instead of N and derived placement instead of hand-partitioning. Claiming a speedup would be wrong.

## Proposal

### User Stories (Optional)

- **As a platform admin**, I describe the fleet-wide workload once — templates, a concurrency budget, and relative cluster capacity — and run one command, instead of computing a partition by hand and running one `kubectl apply` per cluster.
- **As a platform admin with a heterogeneous fleet**, I do not want to recompute replica counts every time a cluster is resized. I want per-cluster budgets derived from what each cluster reports.
- **As an operator draining a cluster for maintenance**, I set its weight to `0` and re-apply. It stops receiving new pools without being removed from the spec, and existing pools are left alone until I remove them.
- **As an operator during an incident**, I want a degraded cluster to fall out of placement on its own, without a health-check protocol or cross-cluster probing.
- **As an RL engineer**, I claim a sandbox by template name and let the client resolve which cluster currently hosts that template, rather than tracking placement myself.
- **As an operator debugging a run**, I want to ask "which clusters are healthy, what does each hold, and what did the planner decide" without Kubernetes credentials for every cluster.

### High-Level Design

#### Components

| Component | Form | Runs where | Talks to |
| --- | --- | --- | --- |
| `fleet-member` | Single-replica Deployment, Python container reusing `k8s_agent_sandbox.SandboxClient` | One per fleet cluster, in the `multi-cluster-fleet` namespace | Object storage + its **own** local apiserver only |
| `fleetctl` | Stateless CLI, one invocation per apply | Admin host — laptop, jump box, or CI. Never in-cluster | Object storage only |
| Fleet bucket | One object-storage bucket per fleet | — | — |

The critical property is that **no component talks to another cluster's apiserver.** A member reconciles only its own cluster; the planner holds no Kubernetes credentials at all. That is what removes the need for a hub, cross-cluster networking, and fleet-wide credential distribution.

End to end:

```
FleetSpec.models[].template_name            (admin authors YAML)
       │
       ↓   fleetctl apply → object storage
       │
Assignments.clusters[X].pools[]             (planner decides which cluster gets what)
       │
       ↓   each cluster's fleet-member polls every 30 s
       │
SandboxWarmPool CRs                         (fleet-member creates them locally on X,
       │                                     referencing admin-managed SandboxTemplates)
       ↓   upstream SandboxWarmPool controller
       │
N warm Sandbox CRs → N warm Pods            (ready, waiting for adoption)
       │
       ↓   user code: SandboxClient.create_sandbox(warmpool=…)
       │
SandboxClaim → adopts a warm Sandbox → served
```

`SandboxTemplate` objects are pre-applied by the admin on every fleet cluster via `kubectl` or GitOps. **The fleet layer never creates templates**; it only references them by name and verifies they exist before creating a pool. Pod spec, resources, and env stay in the `SandboxTemplate` CR where they already live.

#### Coordination Substrate

Three object kinds in one bucket:

| Object | Writer | Reader | Cadence |
| --- | --- | --- | --- |
| `fleet/assignments.json` | `fleetctl apply` | every member | on apply |
| `fleet/capacity/<cluster>.json` | that cluster's member | `fleetctl` | every 30 s |
| `fleet/spec.yaml` | `fleetctl apply` (archived copy) | humans | on apply |

Assignments carry a monotonic `generation`. Members ignore an assignment older than the generation they have already observed, which makes concurrent or out-of-order applies safe. Capacity reports carry warm-pool depth, ready count, active claims, a node-pressure signal, and the generation that member has observed.

#### Placement Algorithm

Two decisions, both stateless functions of `(spec, capacity reports)`.

**Which cluster hosts each pool.** The default `capacity-aware` selector scores:

```
score(cluster) = weight × ready_ratio / (1 + load × (1 + pressure))
```

| Term | Meaning |
| --- | --- |
| `weight` | Relative capacity, from `cluster_weights` |
| `ready_ratio` | `warmpool_ready / warmpool_depth`, or `1.0` for an empty cluster |
| `load` | `warmpool_depth + planned_replicas` (in-run bookkeeping) |
| `pressure` | Node CPU+memory request/allocatable ratio, clamped to `[0, 1]` |

Two determinism guards sit in front of the scorer:

- **Spread-first pre-pass.** The first N pools go one per cluster in sorted-name order, N being the fresh-cluster count. Extras fall through to the scorer. Without this, a cluster carrying any stale load at plan time is skipped entirely by pure-greedy scoring, and placement oscillates between applies.
- **`min_clusters`.** When set, *all* pools are assigned round-robin across the first N sorted-by-name fresh clusters, ignoring scoring entirely. Fully deterministic across re-applies.

**How large each pool is.** A Hamilton largest-remainder allocation splits `max_concurrent` across the placed clusters by weight, summing to exactly the requested total. Then per pool:

```
replicas = clamp(round(cluster_budget × tasks_pool / tasks_total),
                 1,
                 min(tasks_pool, max_pool))
```

Three knobs, and the smallest binds: `max_concurrent` (fleet-wide warm budget), `target_tasks` (per-pool proportional share), `max_pool` (hard per-pool cap). For direct control, set `max_pool` to the size you want and `target_tasks >= max_pool`.

Setting `cluster_weights` to the per-cluster targets themselves makes placement *exact* rather than approximate, because Hamilton normalises — budget equals target. Both scale runs landed on every target with zero overshoot this way.

#### Failure Handling

Staleness-based, with no health-check protocol and no cross-cluster probing. A capacity report older than **90 seconds** excludes that cluster from the next placement pass. A degraded cluster stops receiving new pools; pools it already holds are left untouched, since the fleet layer cannot safely distinguish "cluster is dying" from "member pod is briefly restarting."

Members are self-healing rather than transactional: a reconcile pass that partially fails re-arms itself and retries on the next tick, rather than waiting for the spec to change. This matters because `assignments.json` only changes when a human runs `fleetctl apply` — a member that skipped a pool because its template had not been applied yet would otherwise serve the wrong pool set indefinitely.

#### Example FleetSpec

```yaml
generation: 6                    # bump on every edit; members ignore older generations
max_concurrent: 500              # fleet-wide budget of WARM sandboxes
                                 # (warm capacity, not total running pods)
max_pool: 100                    # cap on any one warm pool's size
placement_policy: capacity-aware
min_clusters: 5                  # optional anti-affinity floor (0 = disabled)

cluster_weights:                 # relative capacity; 0 drains a cluster in place
  cluster-1: 1.0
  cluster-2: 1.0
  cluster-3: 1.0

models:
  # Each entry names a SandboxTemplate the admin has pre-applied on every
  # target cluster. The fleet-member only creates the SandboxWarmPool that
  # points at it.
  - template_name: sb-tmpl-a
    target_tasks: 100            # concurrent claims this pool is expected to serve;
                                 # the input the planner uses to size the pool
    # image: OPTIONAL — only for placement_policy: image-affinity, which hashes
    #        the image name mod N for image-pull locality.
```

The admin does **not** pick clusters, warm-pool names, per-cluster budgets, or per-pool replica counts. The planner derives all four.

#### API Changes

**None.** This KEP adds no fields to `agents.x-k8s.io/v1beta1` or `extensions.agents.x-k8s.io/v1beta1`, and registers no new CRD.

`FleetSpec` and `Assignments` are file schemas in object storage, not Kubernetes resources. They are deliberately *not* CRDs: making them CRDs would require a cluster to host them, which reintroduces the hub apiserver this design exists to avoid.

The fleet layer consumes the existing extension APIs unchanged — it creates `SandboxWarmPool` objects referencing operator-managed `SandboxTemplate` objects, via `CustomObjectsApi` and the existing Python SDK. Both wire schemas are versioned by the `generation` field rather than by Kubernetes API versioning.

Two SDK gaps were found while building this and are worth filing separately: `SandboxClient` exposes no template/warm-pool CRUD (so the member uses `CustomObjectsApi` directly), and `list_sandbox_claims` accepts no `limit`/`continue` (so it cannot be paged at density).

#### Implementation Guidance

The implementation lives under [`fleet/`](../../../fleet/) and is complete; this KEP documents a design that has shipped as an example rather than proposing new work. Layout:

| Path | Contents |
| --- | --- |
| `fleet/python/agent_sandbox_fleet/planner.py` | `plan()` — the stateless `(spec, inventory) → assignments` function |
| `fleet/python/agent_sandbox_fleet/placement.py` | The five selectors and `PlannerRegistry` |
| `fleet/python/agent_sandbox_fleet/budget.py` | Hamilton largest-remainder split |
| `fleet/python/agent_sandbox_fleet/sizing.py` | Per-pool replica calculation |
| `fleet/python/agent_sandbox_fleet/fleet_member.py` | The daemon: reconcile loop, capacity loop |
| `fleet/python/agent_sandbox_fleet/cli.py` | `fleetctl apply` / `status` / `show-assignments` / `show-registry` |
| `fleet/deploy/` | Member Deployment, RBAC, image build |

Notes for anyone extending it:

- **`load_registry()` is the inventory seam.** Everything downstream touches only the in-memory `PlannerCluster` dataclass, so swapping the inventory source is a single, well-isolated change. This is what makes ClusterProfile adoption (below) small.
- **Auth.** The member uses Workload Identity for object storage and its own ServiceAccount token for the local apiserver. Shipped manifests should default to Workload Identity; the key-file variant exists only for local `kind` demos where WI is unavailable.
- **Page every list that scales with cluster size.** An unbounded list is not truncated — the apiserver returns everything — but at density it is a large etcd range read and a multi-megabyte body on a loaded control plane, which is exactly where it times out. An unpaged pod list OOM'd a member at 200K pods.

**Follow-ups before this would be more than an example:**

- Multi-replica member with Lease-based leader election.
- Exponential backoff on all object-storage calls, honouring `Retry-After`, degrading to last-known-good during an outage.
- A Prometheus `/metrics` endpoint: reconcile counts, capacity-report age, warm-pool depth, storage op duration.
- Populate the currently-stubbed `claim_p90_ms` from the sandbox controller's metrics.
- An operational runbook: onboard a cluster, drain a cluster, roll the member, debug a stale report.

**Adopt SIG-Multicluster `ClusterProfile` as the cluster registry.** Today the cluster list is hand-authored in `cluster_weights` and live state is a private JSON schema — a private solution to a problem [KEP-4322](https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/4322-cluster-inventory) has already standardised. The mapping is nearly one-to-one (cluster → one `ClusterProfile`; capacity report → `status.properties[]`; endpoint → `status.accessProviders[]`). Because `ClusterProfile` objects are Kubernetes resources, adopting them fully implies a hub, so adopt in stages — **consume** first (read inventory from a hub; smallest useful increment), then **publish**, then **connect**. Two gaps found while mapping are worth raising upstream: there is no heartbeat property (`ControlPlaneHealthy` only updates on transition, so a silently-dead cluster still reads `True`), and no well-known capacity properties exist, so warm-pool depth, ready ratio, active claims, and node pressure all need a vendor prefix.

## Scalability

This section is measurement, not estimate. Six GKE clusters, 4,567 nodes, gVisor sandbox node pools at `max-pods 256` on 8-vCPU nodes, 500 `SandboxTemplate` objects applied identically to every cluster, 3,000 warm pools total.

| Path | Sandboxes | Wall clock | Notes |
| --- | --- | --- | --- |
| Raw CRs (`kubectl`, no fleet layer) | 1,076,550 | 46.8 min | Held flat 1 h |
| Python SDK, fleet-resolved | 1,075,000 | 58.3 min | 1M crossed at 34.9 min |
| One `fleetctl apply` | 1,069,000 | 116 min | Reproduced 2×; 750K at 12.45 min (946 pods/s); 940K across 5 clusters at 24.9 min |

Four findings that shape the design:

1. **The fleet layer costs about 2.5 seconds.** Planning and publishing 3,000 pools takes ~2.5 s; every member then materialises its 500 warm pools in ~85 s, overlapping the fill. Everything after that is the upstream controller doing identical work on all three paths. This is parity with hand-partitioning, not a speedup — the value is one command and derived placement.

2. **Placement was exact, twice.** All six clusters landed on their targets with no overshoot across two independent runs, when `cluster_weights` was set to the targets themselves.

3. **The long pole is one cluster, not the algorithm.** Five of six clusters finished at 24.9 min. The 116-minute fleet clock is entirely attributable to one cluster that sustained 18.6 object creates/s against a 129,000 target — 129,000 ÷ 18.6/s ≈ 116 min accounts for the whole number. Root cause was its control plane: GKE sizes the apiserver by node count, so the *smallest* cluster in the fleet got 16 apiserver cores where the others had 64, and its controller was APF-starved (113,037 queue-full rejections). Note the perverse consequence: the planner had already given that cluster the smallest share *because* it was smallest, and that was still too much. **Control-plane class is a provisioning property the planner cannot see, and nothing in the capacity report predicts it.** This is the strongest argument for deriving weights from a real cluster inventory rather than an admin-authored number.

4. **`status.readyReplicas` is not a completion signal.** Under load it does not merely lag — it stops. One cluster froze at 62,604 for 14 minutes while still filling, under-reporting by 51%. Read each pod's own `Ready` condition instead; `lastTransitionTime` gives wall clock for free. Counting `status.phase=Running` is not a substitute either, because terminating pods keep that phase and get double-counted.

Steady-state cost is small and bounded: one object-storage write per cluster per 30 s, one read per cluster per 30 s, and a planner pass that is `O(pools × clusters)` in memory on an admin host. The member's own apiserver load is a paged warm-pool list per tick plus, in `full` capacity mode, a sandbox list and a node/pod walk — the latter two are `O(cluster)` and can be disabled with `--capacity-detail=light`, which reports warm-pool depth and readiness only. That is what the capacity-aware planner actually consumes.

Known limits: single-replica members mean a member outage stops reconciliation on that cluster until the Deployment recovers (pools already created keep serving). The 30 s poll bounds how fast a re-apply propagates. The design assumes uniform node pools within a cluster and one bucket per fleet.

## Alternatives (Optional)

| Alternative | Why not |
| --- | --- |
| **Karmada / KubeFleet / OCM** | A full additional control plane to install, operate, and upgrade, for a problem that is "pick a cluster for each warm pool." Contradicts the goal of adding no new control plane. |
| **Kueue MultiKueue** | Batch queue semantics: quota-gated rather than capacity-aware, and dispatching arbitrary CRDs needs compiled-in Go adapters. Wrong shape for stable-identity, long-lived sandbox workloads. |
| **A new multi-cluster controller in this repo** | Over-engineered for batch/RL, and it would need a hub cluster to run in. The whole placement algorithm is a stateless pure function; a controller adds reconciliation semantics we do not need. |
| **Extend `SandboxTemplate`/`SandboxWarmPool` with fleet fields** | Still needs a coordination substrate to decide placement, so it adds API surface without removing the hard part — and it puts multi-cluster concepts into a single-cluster API. |
| **A hub apiserver holding `FleetSpec`/`Assignments` CRDs** | Better ergonomics (`kubectl get fleetspec`) at the cost of a cluster to run, secure, and upgrade, plus fleet-wide credentials to reach it. Object storage already provides durability, versioning, and atomic reads. Deliberately reconsidered under the ClusterProfile staging above. |

**Not rejected — SIG-Multicluster `ClusterProfile`.** It is an inventory schema rather than a control plane, and it is complementary: it would replace the hand-authored `cluster_weights` and the private capacity-report schema while leaving the placement algorithm untouched. Adoption plan in [Implementation Guidance](#implementation-guidance).
