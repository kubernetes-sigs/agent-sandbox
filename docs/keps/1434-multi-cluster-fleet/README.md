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
    - [Client-Side Resolution](#client-side-resolution)
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

There is no hub apiserver, no shared etcd, no cross-cluster networking, and no new CRD. The fleet layer only decides *which cluster gets which warm pool*; the existing `SandboxWarmPool` and `SandboxTemplate` controllers do all the actual work. It would ship as a reference example and Python library under a new top-level `fleet/` directory, not as part of the core controller.

That directory does not exist in the repository yet. The implementation is proposed in a companion PR; this KEP is written to stand on its own and deliberately does not link into `fleet/`, so it stays valid whichever order the two merge in. See [Implementation Guidance](#implementation-guidance).

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
- **As an operator draining a cluster for maintenance**, I set its weight to `0` and re-apply. It stays in the spec and keeps reporting, but takes no new pools *and* sheds the warm capacity it is currently holding. Sandboxes already claimed on it keep running. See [Failure Handling](#failure-handling) for exactly what a drain does and does not delete.
- **As an operator during an incident**, I want a degraded cluster to fall out of placement on its own, without a health-check protocol or cross-cluster probing.
- **As an RL engineer**, I claim a sandbox by template name and let the client resolve which cluster currently hosts that template, rather than tracking placement myself. See [Client-Side Resolution](#client-side-resolution) for the contract this implies.
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

#### Client-Side Resolution

The upstream `SandboxClient` is single-cluster and stays that way. Fleet-aware claiming is a **thin wrapper around it**, not a change to it, and it is optional — a caller that already knows which cluster it wants keeps using `SandboxClient` directly.

The contract:

1. Read `fleet/assignments.json` from the fleet bucket (cached, with an on-demand staleness check; no background threads).
2. Find the clusters whose assignment contains a pool for the requested `template_name`. A template may be on several clusters — spread-first and `min_clusters` both do that deliberately. Resolution strategy is caller-selected: round-robin (default, load-balances), `first` (sorted by name, reproducible in tests), or `hash` (pins a template to one cluster, mirroring `image-affinity` on the client side).
3. Load that cluster's kube context and construct — or reuse from cache — a normal `SandboxClient` against it.
4. Issue an ordinary `SandboxClaim` naming the warm pool, and **watch it on that same cluster**. There is no cross-cluster watch: once resolution picks a cluster, everything after it is a single-cluster operation against a single apiserver.

**Where `template_name` comes from — it is an input, not something read off a claim.** The caller supplies it: `resolve_cluster(template)` for the pure lookup, or `FleetSandboxClient.create_sandbox(template=...)` for the wrapper. Resolution runs **before any `SandboxClaim` object exists**, and its output is the cluster the claim will then be created on. The fleet layer never performs the inverse lookup — given a claim, which cluster is it on? — because it does not need to: it chose that cluster itself, and everything after resolution is a single-cluster operation.

This is what makes resolution API-version-independent, and it is the reason the contract is stated in terms of templates rather than pools. `template_name` is a key into `fleet/assignments.json`, matched against the `template` field of each planned pool. It is not a claim field, and it is deliberately *not* the warm-pool name, which the planner derives and the admin never types.

**Which claims this applies to — all of them, on both served versions.** The claim is written by the SDK on the already-resolved cluster, so whichever version it writes, and whatever the claim can or cannot do once it lands, resolution has already happened. Concretely:

- `extensions.agents.x-k8s.io/v1beta1` (storage): `warmPoolRef` is `+required`; there is no `templateRef`. Going from a claim back to a template means a lookup — `warmPoolRef.name` → `SandboxWarmPool.spec.sandboxTemplateRef.name` — which the fleet layer never has to do.
- `extensions.agents.x-k8s.io/v1alpha1` (`served: true, storage: false`): `templateRef` is required and `warmpool` is an optional policy taking `none`, `default`, or a specific pool name. The conversion webhook normalises all of it into a `v1beta1` `warmPoolRef`, on two branches (the `WarmPool`/`TemplateRef` → `WarmPoolRef` conversion in `extensions/api/v1alpha1/sandboxclaim_conversion.go`): a specific pool passes through unchanged; otherwise, if a bound sandbox already exists whose name differs from the claim's, the pool name is recovered from it, and only failing that is `shadow-pool-<templateRef.Name>` synthesised. Note the second branch is keyed on whether a sandbox is bound, **not** on the policy value, so `none` and `default` convert identically.

Cold starts do not bypass any of this either. Two `v1beta1` spec fields force one — `env`, because the warm pods were started without those variables, and `volumeClaimTemplates`, because warm pods have no such volumes. A claim carrying either still names a warm pool and is still served on the resolved cluster; the controller cold-starts it from that pool's `SandboxTemplate` instead of adopting a warm replica. That works precisely because the template exists on the resolved cluster, which is what the assignment guarantees.

What cold-start traffic does change is *accounting*, and that belongs in the spec rather than in the resolver. `target_tasks` sizes a pool for claims that adopt, so cold-start claims against a pool draw down none of the warm depth it was sized for: the pool stays full while those claims wait on a fresh pod. A predominantly cold-start workload should therefore get its own template with a small `target_tasks` rather than inflating a pool nothing consumes. Round-robin resolution is also the wrong default for it — round-robin balances warm depth, which these claims do not touch — so `hash` is the better strategy there.

Failure modes are explicit rather than silent: no cluster hosting the template raises a distinct error (`fleetctl apply` not yet run, a typo, or the referenced `SandboxTemplate` CR missing on every cluster, so no member could create a pool for it). Bucket reads retry with backoff so a storage blip does not fail a claim.

The caller supplies credentials for the clusters it intends to claim on — the resolver distributes placement information, never credentials.

#### Coordination Substrate

Three object kinds in one bucket:

| Object | Writer | Reader | Cadence |
| --- | --- | --- | --- |
| `fleet/assignments.json` | `fleetctl apply` | every member | on apply |
| `fleet/capacity/<cluster>.json` | that cluster's member | `fleetctl` | every 30 s |
| `fleet/spec.json` | `fleetctl apply` (archived copy) | humans | on apply |

Capacity reports carry warm-pool depth, ready count, active claims, a node-pressure signal, and the generation that member has observed.

**Three separate concerns, three separate fields.** An earlier draft of this design used one integer, `generation`, for all three. That conflation is a defect, so they are split:

| Field | Answers | Written by | On mismatch |
| --- | --- | --- | --- |
| `schema_version` | "can I parse this?" | the writer's code version | member refuses the payload and keeps serving its current pool set, loudly |
| `generation` | "is this newer than what I have?" | derived by `fleetctl apply` | member ignores anything not greater than what it has observed |
| store object generation | "did someone else write since I read?" | the object store | the apply is rejected and retried against the new state |

- **`schema_version` is a compatibility gate, not an ordering token.** A higher `generation` tells an older member nothing about whether it can understand the payload. Members accept a payload whose `schema_version` they know and reject one they do not; unknown *fields* within a known version are ignored, so additive changes do not require a version bump. Refusing is the safe failure: a member that cannot parse an assignment must not conclude the assignment is empty and tear down every pool it holds. That holds even when the refusal arrives on a fresh process start, before this process has parsed any assignment: with no last-good set to fall back on, the member skips reconciliation entirely rather than reading the refusal as an empty plan.
- **`generation` is derived, and `fleet/assignments.json` is where it lives.** `fleetctl apply` reads the currently published assignment, takes its `generation`, increments, and publishes the result; with no assignment object present yet, the first apply is generation `1`. It is deliberately *not* read back out of the archived `fleet/spec.json`. The archive is a human-readable record of what was applied, written after the plan succeeds — making it the counter would put the authoritative value in the one object nothing in the system reads, and desynchronise the fleet the first time an archive write failed after a successful publish. Keeping the counter on the assignment also keeps it on the same object as the compare-and-set precondition below and the same object members compare against, so ordering, storage, and consumption all agree. The archived spec is stamped with the generation that was applied (as `applied_generation`, a separate key from the deprecated spec-input field), for traceability only. A hand-bumped integer is a silent-failure footgun: forget the bump and every member correctly ignores the apply, with no error anywhere — the operator sees a successful `fleetctl apply` and no change in the fleet. An explicit `--generation` override remains for replay and disaster recovery, and `fleetctl apply` refuses to publish a generation that is not greater than the one already in the bucket.
- **Ordering under concurrent applies is enforced by the store, not by the integer.** Two admins applying at once can otherwise derive the same next generation from the same base and race, and last-writer-wins silently discards one plan. Assignments are written with a compare-and-set precondition on the object's own store-side generation (`ifGenerationMatch` on GCS, `If-Match` on an S3-compatible store), so the loser's write fails and is retried against the state the winner produced. The monotonic `generation` remains what *members* compare, because a member sees only the payload, never the store metadata.

Both files are read atomically: the read pins the object generation across fetch and body download, so a member never pairs new bytes with an old revision.

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

`placement_policy` selects among six selectors. The other five: `image-affinity` (MD5 of the image modulo the sorted cluster count, so a given image always lands on the same cluster for pull locality; falls back to least-loaded when that cluster is out of capacity), `least-loaded` (fewest active claims, with *unmeasured* sorting last rather than as zero, so a cluster whose claim list failed cannot win the tiebreak by looking idle), `capacity-weighted` (static `weight / (1 + active_replicas)` — the predecessor of `capacity-aware`, kept for parity with the PoC), `round-robin` (cycles eligible clusters in stable order), and `pinned` (each model names its target cluster in the spec, for fleets whose data layer is pre-sharded per cluster — the planner honours the name directly rather than scoring). `capacity-aware` is the default and the policy every scale run reported below used.

**Eligibility, defined once.** A cluster is **eligible** for placement when *both* hold:

- its capacity report is fresher than 90 s (see [Failure Handling](#failure-handling)), and
- its weight is greater than zero.

Eligibility is evaluated **before** any of the three placement paths below, so all three honour it identically. This matters because scoring alone does not enforce it: a zero-weight cluster scores zero and so never *wins* the scorer, but the spread-first pre-pass and the `min_clusters` round-robin do not consult the scorer at all. A drain therefore has to be a filter on the candidate set, not a consequence of a low score — otherwise `weight: 0` still receives one pool per cluster, and the min-1 replica floor below turns that into a live pool on a cluster the operator asked to drain.

`cluster_weights` is a map of *explicit* weights. Two cases that look similar and are not:

- **`cluster_weights` omitted or a cluster absent from it** — no preference expressed. Default weight `1.0`; the cluster is eligible.
- **Every listed cluster explicitly weighted `0`** — every cluster drained. The plan is empty and the fleet tears down. This is a valid instruction, not a degenerate input, and must not be reinterpreted as "no preference, split evenly" — that inverts a full drain into a full deployment.

Two determinism guards then sit in front of the scorer:

- **Spread-first pre-pass.** The first N pools go one per cluster in sorted-name order, N being the eligible-cluster count. Extras fall through to the scorer. Without this, a cluster carrying any stale load at plan time is skipped entirely by pure-greedy scoring, and placement oscillates between applies.
- **`min_clusters`.** When set, *all* pools are assigned round-robin across the first N sorted-by-name eligible clusters, ignoring scoring entirely. Fully deterministic across re-applies.

  `min_clusters` is a floor on *spread*, not a precondition. When it exceeds the eligible count it is clamped to that count and a warning names both numbers; the apply proceeds. Failing instead would mean a routine event — one cluster going stale mid-incident — takes the whole fleet's plan down with it, which is precisely when re-planning matters most. When no cluster is eligible the plan is empty, and that is reported rather than inferred.

**How large each pool is.** A Hamilton largest-remainder allocation splits `max_concurrent` across the placed clusters by weight, summing to exactly the requested total. Then per pool:

```
replicas = clamp(round(cluster_budget × tasks_pool / tasks_total),
                 1,
                 min(tasks_pool, max_pool))
```

Three knobs, and the smallest binds: `max_concurrent` (fleet-wide warm budget), `target_tasks` (per-pool proportional share), `max_pool` (hard per-pool cap). For direct control, set `max_pool` to the size you want and `target_tasks >= max_pool`.

**Preconditions, rejected at spec load.** `max_concurrent`, `max_pool`, and every `target_tasks` must be `>= 1`; `cluster_weights` values must be finite and `>= 0`. These are schema constraints, validated before planning, so the formula above cannot be reached with a zero denominator or a non-positive cap. Rejecting at load is deliberate: the error can name the file and the offending key, where the same bad value surfacing from inside the allocator produces an `OverflowError` with no indication of which cluster is at fault.

**`max_concurrent` is a target, not a hard cap, and the gap is the lower clamp.** Every placed pool gets at least one replica, because a pool assigned zero replicas can only be served cold, which makes the assignment pointless. When a cluster holds more pools than its budget slice, the floor wins and the slice is exceeded — `max_concurrent: 1` across two pools yields two replicas, not one. The planner warns with the cluster name, the planned total, the slice, and the two ways out (raise `max_concurrent`, or lower `min_clusters` to place fewer pools per cluster). It does not silently truncate, and it does not silently overshoot without saying so. An operator who needs a genuine ceiling should set `max_pool` and keep pool count under budget.

**When Hamilton is exact.** Setting `cluster_weights` to the per-cluster targets themselves yields those exact budgets only when all four preconditions hold:

- **integrality** — every target is a whole number,
- **sum match** — `sum(targets) == max_concurrent`,
- **completeness** — every eligible cluster appears in `cluster_weights`, and
- **eligibility** — every weighted cluster is eligible and receives at least one pool.

Hamilton *normalises* — it splits `max_concurrent` across the weights of the clusters actually placed, so exactness is a property of the **resolved** weight map, not of the map the admin typed. Break any precondition and the targets are rescaled rather than honoured.

The integrality precondition holds trivially when weights *are* targets, because a target is a sandbox count, but it has to be stated: `hamilton_split` returns integers, so a fractional weight can never be honoured as written. Weights of `1.5` and `1.5` against `max_concurrent: 3` allocate `2` and `1`, not `1.5` each — largest-remainder breaks the tie in favour of whichever key sorts first, which is arbitrary from the admin's point of view. Fractional weights are still perfectly sound as *ratios*, which is their intended use; they are only unsound when read as literal per-cluster budgets. Note that integer targets survive the float arithmetic inside the split even at fleet scale: `total * (w / tw)` can land a hair under a whole number, but the resulting near-`1.0` remainder is precisely what largest-remainder hands the leftover to, so the value lands back on the target. If the sum is 400 and `max_concurrent` is 300, each cluster gets *approximately* three-quarters of its target — proportionally, then rounded, and the rounding is not evenly spread. Skewed weights make that visible: `{a: 1, b: 399}` against `max_concurrent: 300` yields `{a: 1, b: 299}`, so `a` keeps 100% of its target while `b` absorbs the entire shortfall. The three-quarters reading is a good approximation only when every target is large relative to the number of clusters. If a weighted cluster is stale or drained, its weight leaves the denominator and the survivors are scaled *up* to absorb its share.

The completeness precondition is the easiest to miss, because `cluster_weights` reads like a filter and is not one: a cluster absent from it is not weightless, it defaults to `1.0`. Add a seventh cluster to the fleet and forget to list it, and the other six do not stay on their targets — the newcomer arrives at weight `1.0`, joins the split, and rescales everyone. That case and the stale-cluster case are the dangerous ones, because in both the plan looks like it worked. Both scale runs landed on every target with zero overshoot because all four preconditions held; the claim is conditional, not general.

#### Failure Handling

Staleness-based, with no health-check protocol and no cross-cluster probing. A capacity report older than **90 seconds** excludes that cluster from the next placement pass, exactly as a `weight: 0` drain does.

**Exclusion is not omission, and that is the whole contract.** An excluded cluster is written into the assignment with an *empty* pool set, and an empty pool set means "drop everything". There is only one exclusion mechanism and one outcome, so a drain and a stale cluster behave identically: as soon as that cluster's member next reads the file, its warm pools go away.

**What that deletes, precisely.** Deleting a `SandboxWarmPool` deletes the warm, unclaimed sandboxes it owns. It does **not** touch sandboxes that have already been claimed, because adoption re-parents them: the claim controller clears the sandbox's owner references and makes the `SandboxClaim` the controller, so the pool is no longer an owner by the time work is running on it. A drain therefore removes idle warm capacity and leaves in-flight work alone, which is what makes it usable for maintenance — cordon the cluster with `weight: 0`, let claims drain naturally, then take it out. What it is *not* is a way to park a cluster with its warm capacity intact; re-applying with a non-zero weight is what refills it, and that refill is a cold fill.

**The sharp edge is the trigger, not the behaviour.** Teardown on exclusion is correct when the cluster is genuinely gone, and it is what lets a drain empty a cluster that has already stopped reporting. But the staleness trigger is a missing *capacity report*, not a missing member, and those are different failures. A cluster whose reconcile loop is perfectly healthy and whose publish path is broken — bucket permissions, a wedged capacity thread — will read that empty assignment and shed every warm pool it holds while looking fine from inside. The planner logs the teardown with the cluster name and report age rather than performing it silently. Narrowing it further (a separate liveness signal, or a grace period distinguishing "briefly restarting" from "gone") is listed under follow-ups.

Two guards bound the blast radius:

- **A fleet-wide teardown is never the response to an absence of data.** If *no* cluster has a fresh report, the planner refuses to publish and exits non-zero, rather than emitting an empty plan that would drop every warm pool on every cluster. An empty plan requires an affirmative instruction — every cluster explicitly weighted `0`.
- **`fleetctl apply` writes assignments before archiving the spec, and only if planning succeeded.** A failed plan leaves the bucket describing the last plan that actually ran.

Members are self-healing rather than transactional: a reconcile pass that partially fails re-arms itself and retries on the next tick, rather than waiting for the spec to change. This matters because `assignments.json` only changes when a human runs `fleetctl apply` — a member that skipped a pool because its template had not been applied yet would otherwise serve the wrong pool set indefinitely.

#### Example FleetSpec

```yaml
schema_version: 1                # payload shape; members reject what they can't parse
max_concurrent: 500              # fleet-wide budget of WARM sandboxes
                                 # (warm capacity, not total running pods)
max_pool: 100                    # cap on any one warm pool's size
placement_policy: capacity-aware
min_clusters: 2                  # optional anti-affinity floor (0 = disabled).
                                 # Clamped to the eligible-cluster count, with a
                                 # warning, when it exceeds it.

cluster_weights:                 # Relative capacity. List EVERY cluster: an omitted
  cluster-1: 1.0                 # one is not weightless, it defaults to 1.0 and still
  cluster-2: 1.0                 # joins the split. 0 drains a cluster in place -- it
  cluster-3: 0.0                 # stays in the spec and keeps reporting, takes no new
                                 # pools, and sheds the warm capacity it holds.
                                 # Already-claimed sandboxes keep running.

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

The admin does **not** pick clusters, warm-pool names, per-cluster budgets, per-pool replica counts, or the assignment `generation`. The planner derives all five. Note there is no `generation` key in the spec above: `fleetctl apply` reads the generation of the currently published `fleet/assignments.json` and increments it, then stamps the value it used into the archived copy of this spec. The archive records what was applied; it does not drive it. See [Coordination Substrate](#coordination-substrate).

#### API Changes

**None.** This KEP adds no fields to `agents.x-k8s.io/v1beta1` or `extensions.agents.x-k8s.io/v1beta1`, and registers no new CRD.

`FleetSpec` and `Assignments` are file schemas in object storage, not Kubernetes resources. They are deliberately *not* CRDs: making them CRDs would require a cluster to host them, which reintroduces the hub apiserver this design exists to avoid.

The fleet layer consumes the existing extension APIs unchanged — it creates `SandboxWarmPool` objects referencing operator-managed `SandboxTemplate` objects, via `CustomObjectsApi` and the existing Python SDK. Claims are written through the SDK rather than constructed per-version, so both `extensions.agents.x-k8s.io/v1alpha1` and `v1beta1` claims are served — resolution happens before the claim exists and never inspects one: see [Client-Side Resolution](#client-side-resolution).

Of the three object-storage schemas, the two the planner authors — `fleet/spec.json` and `fleet/assignments.json` — are versioned by `schema_version` rather than by Kubernetes API versioning. `generation` orders successive plans within one schema; it is not a compatibility marker, and a reader must not infer parseability from it.

The third, `fleet/capacity/<cluster>.json`, deliberately carries no `schema_version`, because a version gate would buy it nothing a weaker guarantee does not already provide. It is telemetry, not a command: the planner reads it defensively, skipping any report that is not a JSON object or that names no cluster, and every derived signal is optional — `active_claims` and `node_pressure_score` are `None` when unmeasured, which is explicitly not `0`. A report the planner cannot parse is simply skipped, and a cluster with no fresh report ages out of the registry after 90 s and drops out of placement. That is the same safe direction refusal gives the assignment, reached without a version negotiation. The cost is real and worth naming: an incompatible reshaping of the report would degrade the fleet to unweighted placement rather than fail loudly, so adding `schema_version` here is the right move the first time that shape changes incompatibly. Additive fields need no gate in either case — unknown keys are ignored.

Two SDK gaps were found while building this and are worth filing separately: `SandboxClient` exposes no template/warm-pool CRUD (so the member uses `CustomObjectsApi` directly), and `list_sandbox_claims` accepts no `limit`/`continue` (so it cannot be paged at density).

#### Implementation Guidance

The implementation is written and validated at the scale reported below, but it is **not in this repository yet** — it is proposed in a companion PR that adds a new top-level `fleet/` directory. Nothing in this KEP links into that directory, so this document does not break if the two land out of order or if the layout below changes during review. Proposed layout:

| Path | Contents |
| --- | --- |
| `fleet/python/agent_sandbox_fleet/planner.py` | `plan()` — the stateless `(spec, inventory) → assignments` function |
| `fleet/python/agent_sandbox_fleet/placement.py` | The six placement selectors and `PlannerRegistry` |
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

- Distinguish "member cannot publish" from "cluster is gone", so a broken capacity-report path does not tear down a healthy cluster's pools (see [Failure Handling](#failure-handling)). A grace period, or a liveness signal separate from the capacity report.
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

4. **`status.readyReplicas` should not be used as a benchmark completion signal.** This is a statement about *measurement methodology*, not a claim that the field is wrong or that the pool-readiness contract should change: `readyReplicas` is the API's ready-state count and remains the right thing for a client to consume. But it is produced by a status-aggregation path that competes with the create path for the same starved control plane, and under load it does not merely lag — it stops. One cluster froze at 62,604 for 14 minutes while still filling, under-reporting by 51%; the pods were ready and serving the whole time. Anything timing a fill must therefore count each pod's own `Ready` condition directly, where `lastTransitionTime` also gives wall clock for free. Counting `status.phase=Running` is not a substitute either, because terminating pods keep that phase and get double-counted. The fleet's own capacity reports carry both the aggregated depth and the directly-counted ready total for exactly this reason.

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
