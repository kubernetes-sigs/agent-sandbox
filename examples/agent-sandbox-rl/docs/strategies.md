# Strategies, sizing & performance tuning

The single reference for **how to run a batch fast**: the warm-pool **strategies**,
the **sizing modes**, the **caching / infra levers**, which **workload** each fits,
and the exact **flag** for each. The README has the quickstart; this is the deep doc.

There are **three independent axes**. Pick one from each:

1. **Warm-pool strategy** — *when and how many* pools exist (`naive` / `sliding` /
   `pipelined` / `none`). [§2](#2-warm-pool-strategies-whenhow-many-pools)
2. **Sizing mode** — *how deep* each pool is (concurrency-aware default vs
   instant-claim `warm_per_task` + `colocate_replicas`). [§3](#3-sizing-modes-how-deep-each-pool-is)
3. **Caching / infra levers** — *how to avoid or hide the image pull* (node cache,
   epochs, pre-pull, mirror, streaming, disk). [§4](#4-caching--amortization-levers-avoid-the-pull) / [§5](#5-gke-infra-levers-node-pool-side)

At scale, wall-clock for SWE-bench-style fleets is dominated by **image pull**
(`wait_pool_ready` in the RunReport), not task work — a full SWE-bench-Verified run
(500 multi-GB images) is ~99 % pull. Most infra levers live *outside* this package
(node-pool / registry settings); the package exposes small knobs that target them.

---

## 1. Pick your config (decision table)

The two workloads pull in opposite directions:

- **Eval** — a **1:1** sweep (many distinct images, **one task each**). *Pull-bound.*
  The job is to overlap and amortize pulls.
- **RL** — **1:G** (one problem image, **G rollouts** claimed at once). *Claim-bound
  per problem.* The job is to give every rollout an instant, ready sandbox.

| Workload | Strategy | Sizing flags | Caching levers | Why |
| :--- | :--- | :--- | :--- | :--- |
| **Eval (1:1 sweep)** | **`pipelined`** | default (concurrency-aware) | `epochs`/`keep_warm` + `IfNotPresent` + in-region mirror; `prepull` if it fits disk | Pull dominates → overlap window N+1's pull with N's run, and reuse the node cache across passes. `warm_per_task`/`colocate` are no-ops (1 task/image). |
| **RL (1:G rollouts)** | **`naive`** or **`sliding`** | `warm_per_task=True` (+ `max_warmpool_size ≥ G`), `colocate_replicas=True` | `keep_warm=True` across steps + `IfNotPresent` | Every rollout claims its own ready replica → lowest straggler tail. **Avoid `pipelined`** — deep replicas shrink its window and serialize problems. |
| **Tiny run / debug** | `none` | default | — | Lowest footprint (one size-1 pool per image, on demand); inherently sequential. |
| **Batch may exceed pool capacity** | `pipelined` or `sliding` | default | — | Bounded footprint survives over-subscription where `naive` drops tasks (see [§6](#6-capacity--over-subscription)). |

```python
# Eval: 1:1 sweep, pull-bound
fleet.run(eval_fn, strategy="pipelined", epochs=1)

# RL: G rollouts/problem, claim-bound
fleet = SandboxFleet(FleetConfig(
    clusters=[...], max_concurrent=64, max_warmpool_size=16,  # >= rollouts/problem
    warm_per_task=True, template=TemplateSpec(colocate_replicas=True)))
fleet.run(rollout_fn, strategy="naive", keep_warm=True)       # reuse across steps
```

### All levers at a glance

| Lever | What it does | Knob / API | Best for |
| :--- | :--- | :--- | :--- |
| **Warm-pool strategy** | when pools exist & footprint | `run(strategy=…)`: `naive` / `sliding` / `pipelined` / `none` | `pipelined` eval; `naive`/`sliding` RL |
| **Concurrency-aware sizing** (default) | pool depth = image's share of the budget | `max_concurrent`, `max_warmpool_size` | eval (1:1), minimal footprint |
| **Instant-claim (pre-warm)** | one warm replica **per task** | `FleetConfig.warm_per_task=True` | RL (G rollouts share an image) |
| **Replica co-location** | pack a pool's replicas on one node → 1 pull/node | `TemplateSpec.colocate_replicas=True` | deep pools (RL), cache reuse |
| **Cross-epoch reuse** | keep pools resident across passes | `run(epochs=N)` / `keep_warm=True` | repeated passes (RL epochs) |
| **Node layer cache** | re-use already-pulled layers | `TemplateSpec.image_pull_policy=IfNotPresent` (default) | every repeated run |
| **Pre-pull DaemonSet** | cache images on **every** node *before* the hot path | `fleet.prepull()` / `setup(prepull=True)` | spread a fixed set across nodes / autoscale |
| **In-region mirror** | cut cross-region pull + Docker Hub rate limits | `make_rewriter(...)` + `load_tasks(image_rewrite=…)` | Docker Hub-hosted images |
| **Image Streaming** | pods Ready before the full pull | node-pool `--enable-image-streaming` + `node_selector` | large images, task reads a fraction |
| **Disk-aware window** | cap the auto window to node disk | `avg_image_gb` / `node_ephemeral_gb` / `disk_headroom` / `cluster_nodes` | growing the window safely |
| **Bigger / secondary boot disk** | more resident layers / images present at boot | node-pool setting + `node_selector` | fixed image sets, autoscale-warm |

---

## 2. Warm-pool strategies (when/how many pools)

A *strategy* decides **when pools are warmed and how many exist at once**. All run
claim+exec in parallel up to `concurrency`; they differ in footprint and how they
hide pull latency.

| Strategy | Mechanics | Peak footprint | Best for |
| :--- | :--- | :--- | :--- |
| **`naive`** | Warm **every** image up front (concurrently), process all, tear down. | all pools (= #images) | Sets that fit the pool; RL (with `warm_per_task`). Fastest claims once warm. |
| **`sliding`** | Keep a rolling **window** of warm pools tracking the concurrency frontier; warm window N, run it, unwarm, advance. | ~one window (auto = `max_concurrent`) | Large sets on limited disk. |
| **`pipelined`** | Double-buffered sliding window — **prefetch window N+1 while N runs**, so pull overlaps execution. | ≤ 2 windows (window halved so peak ≈ `max_concurrent`) | **Pull-bound eval**; over-subscription (bounded). |
| **`none`** | One size-1 pool per image, on demand, torn down after. | 1 | Tiny runs / debugging (sequential). |

Notes:
- **`pipelined` is the throughput pick when pull dominates** (cold/large images), because
  it hides each window's pull behind the previous window's execution. With warm/cached
  images it ≈ `sliding` (no pull to hide), though it still hides warm-pool *creation*
  latency.
- **Windowed strategies warm a window concurrently** (bounded by `max_concurrent`), so a
  window of pulls isn't serialized — this is what made `sliding`/`pipelined` competitive
  with `naive` at scale.
- **Repeated passes:** `run(..., epochs=N)` runs N passes keeping pools resident between
  them (re-pulls hit the node cache), tearing down once at the end (returns `list[list]`);
  `keep_warm=True` leaves pools up for a caller-driven loop ([§4](#4-caching--amortization-levers-avoid-the-pull)).

---

## 3. Sizing modes (how deep each pool is)

A *sizing mode* decides **how many replicas each pool gets**. Independent of strategy.

### Concurrency-aware (default) — for eval

Pool depth is the image's **share of the concurrency budget**, not its task count:

```text
replicas_image = clamp(round(MAX_CONCURRENT × tasks_image / tasks_total),
                       1, min(tasks_image, MAX_WARMPOOL_SIZE))
```

`max_concurrent` is the one knob that both **sizes pools** and **parallelizes
claim+exec**. This avoids warming *N* pods for *N* tasks while keeping sub-second claims.
For a 1:1 eval sweep each image has one task → one replica.

### Instant-claim (`warm_per_task` + `colocate_replicas`) — for RL

RL samples **G rollouts per problem**, so the *same* image is claimed G times at once;
the 2nd rollout shouldn't queue behind the 1st. Two opt-in levers (both default off):

- **`FleetConfig.warm_per_task=True`** — size each pool to `min(tasks_image,
  max_warmpool_size)`: **one ready replica per task**, so every rollout claims
  immediately. Raise `max_warmpool_size` to ≥ G (it warns and clamps otherwise).
- **`TemplateSpec.colocate_replicas=True`** — a soft `podAffinity`
  (`topologyKey: kubernetes.io/hostname` on the shared `sandbox=<template>` label) so a
  pool's replicas prefer the **same node**. With `IfNotPresent`, only the **first**
  replica pulls the image; the rest start from that node's layer cache — an *N*-replica
  pool becomes **one pull instead of N**. Soft, so it spills to other nodes (each
  re-pulling once) rather than dead-locking when a node fills. Budget node capacity for
  `replicas × cpu_request` (e.g. 50 × 250m ≈ 13 vCPU → an `e2-standard-16`).

What they do and don't buy:
- They cut **claim latency (time-to-sandbox), especially the tail** — what matters in a
  synchronous RL step (you wait for the slowest of G rollouts). They do **not** speed a
  zero-work batch: wall is bounded by `max_concurrent`, which default sizing already
  saturates. In real RL the deeper warm-prep overlaps execution, so the claim win is pure
  upside.
- **No-op for eval** (1 task/image → `min(1, …) = 1` replica, and one replica has nothing
  to co-locate).
- ⚠️ **Pair with `naive`/`sliding`, not `pipelined`.** Deep per-problem replicas shrink
  the pipelined window to keep its footprint bounded, which serializes problems and
  underfills `max_concurrent` once rollouts do real work.

---

## 4. Caching & amortization levers (avoid the pull)

A pull is **fetch compressed blobs → decompress/unpack into the node's snapshot store**.
The decompress/unpack step dominates (not the network — see [§7](#7-measured-findings)),
so **"avoid the pull" beats "speed up the pull."** Reuse the *unpacked* image on the node:

- **Node layer cache** — `TemplateSpec.image_pull_policy` defaults to **`IfNotPresent`**,
  so once an image's layers are on a node, re-creating its pool (next window/epoch) skips
  the pull. Set `Always` only if a tag mutates.
- **Cross-epoch reuse** — `run(..., epochs=N)` keeps pools resident between passes and
  tears down once at the end; `keep_warm=True` leaves them up for your own loop. Epoch 2+
  hits the node cache and `wait_pool_ready` collapses toward the claim+process floor.
- **Replica co-location** — `colocate_replicas=True` (above) is the *within-pool* analogue
  of the cross-epoch cache: one pull per node instead of per replica.
- **Pre-pull DaemonSet** — `fleet.prepull()` (or `setup(prepull=True)`) lays down a
  short-lived DaemonSet with one init container per image (`IfNotPresent`, `sh -c exit 0`),
  so **every** selected node pulls + unpacks **every** image into its containerd cache
  before any claim; it waits until all nodes are ready, then removes the DaemonSet.

  ```python
  fleet.load_tasks(SweBenchSource(limit=500)); fleet.plan()
  fleet.prepull()            # every node caches all 500 images; or setup(prepull=True)
  fleet.run(process_fn, strategy="sliding")   # warm pools now hit the node cache
  ```

  It honors `node_selector` / `image_pull_secret`, runs **per cluster**, and (being a
  DaemonSet) **auto-covers newly-autoscaled nodes**. It's the only lever that spreads
  ready images across **all** nodes ahead of the hot path — unlike `colocate_replicas`
  (one node per pool) or `epochs` (only the nodes a pass scheduled on). Bounded by node
  disk — pair with the disk-aware window ([§5](#5-gke-infra-levers-node-pool-side)).

- **In-region registry / pull-through mirror** — cuts cross-region latency + Docker Hub
  rate limits (though *not* decompress — see [§7](#7-measured-findings)). Redirect tasks
  with the built-in rewriter, no change to the source:

  ```python
  from agent_sandbox_rl import make_rewriter
  fleet.load_tasks(source, image_rewrite=make_rewriter(
      registry="us-docker.pkg.dev", project="PROJECT", repo="REPO"))
  ```

  Either an **Artifact Registry remote (pull-through) repo** (caches Docker Hub in-region
  on first pull, no eager copy) or a **standard repo + eager `crane copy`** (full Docker
  Hub independence). Grant the node SA `roles/artifactregistry.reader`.

  ```bash
  gcloud artifacts repositories create dockerhub-cache \
    --repository-format=docker --mode=remote-repository \
    --remote-docker-repo=DOCKER-HUB --location=us-central2
  ```

---

## 5. GKE infra levers (node-pool side)

These are node-pool / disk settings outside the package; target them with the existing
template knobs.

- **Image Streaming (gcfs)** — with images in Artifact Registry,
  [Image Streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
  lets pods become **Ready before the full image is local** (containerd streams layers on
  demand). Helps when a task touches a *fraction* of a large image; **hurts** warm-pool
  claims that read most of it (see [§7](#7-measured-findings)). Node-pool setting
  (`--enable-image-streaming`); target it with `TemplateSpec.node_selector`.
- **Bigger / faster node disk** — more images resident per node ⇒ larger `window_size` ⇒
  fewer windows. A larger boot disk (or `pd-ssd` / local SSD) holds more layers. Tell the
  sizer so a bigger window can't over-fill nodes:
  ```python
  FleetConfig(avg_image_gb=3.8, node_ephemeral_gb=350, disk_headroom=0.25,
              cluster_nodes=30)   # disk budget spans the whole pool
  ```
  The auto window for `sliding`/`pipelined` is then capped to fit usable disk.
- **Secondary boot disk** — bake the (unpacked) images into a disk image once and attach
  it to the pool, so they're present at node boot — **zero pull, zero unpack** on a fresh
  node. Best fit for a fixed image set (e.g. SWE-bench 500); survives autoscale/node churn.
- **Targeting a node pool** — Image Streaming / secondary boot disk / bigger disks are all
  node-pool properties; route to the right pool per image via the existing template knobs,
  no package-specific code:
  ```python
  FleetConfig(template=TemplateSpec(
      node_selector={"cloud.google.com/gke-nodepool": "streaming-ssd-pool"},
      extra_pod_spec={"tolerations": [{"key": "dedicated", "operator": "Exists"}]}))
  ```

---

## 6. Capacity & over-subscription

Two distinct "too many pods" failure modes — bounded-footprint strategies and correct
sizing avoid both:

- **Pod-count cap.** When the working set exceeds pool capacity, `naive` (peak = #images)
  over-commits and drops tasks; **footprint-bounded `sliding`/`pipelined` survive** (in a
  2000-task / ~3300-pod-cap test, `pipelined` finished ~100 % where `naive` dropped ~25 %).
- **CPU-request slot cap.** Each node fits `(allocatable_cpu − system) ÷ cpu_request`
  sandbox pods (e.g. ~6 on an e2-standard-2 at 250m). If a strategy's peak exceeds the
  pool's total slots, pods go `Pending` → `wait_pool_ready` timeouts. Fix with **more
  nodes, a smaller per-pod `cpu_request`, a smaller peak footprint (window/strategy), or
  bigger nodes**.

Use the **capacity planner** to size this automatically — it probes the pool's CPU /
ephemeral disk / pod density and picks strategy + `max_concurrent` + replicas + window,
reporting the binding bottleneck (see the README "Capacity-aware planning" section).

---

## 7. Measured findings

From the live SWE-bench runs (full reports + numbers consolidated in the private
`performance_reports/` — format documented in
[`../performance_reports/README.md`](../performance_reports/README.md)):

- **Pull is decompress-CPU-bound, not network/disk-bound** — three experiments agree:
  in-region AR mirror ≈ Docker Hub on cold pull (a wash); **Image Streaming was net
  *slower*** for warm-pool claims (cost shifts from pull → first-file-access); **pd-ssd
  gave no cold-pull win** over pd-balanced. → caching that pays caches the *unpacked*
  image (node cache, pre-pull, secondary boot disk).
- **The big wins were:** parallel windowed warm (**~9×** on `sliding` at 500),
  node-cache amortization cold→warm (**~6–7×** on `wait_pool_ready`), and controller +
  client-connection-pool scaling (**~3–6×** claim latency).
- **Instant-claim** cut the per-rollout claim tail (e.g. 12 s → 4 s at 50×4), but not
  batch wall; and **`pipelined` + instant-claim regressed** wall under real rollout work
  (window shrinkage) — hence the "RL → `naive`/`sliding`" rule above.

---

## Putting it together

**Eval (1:1 SWE-bench sweep).** Pull dominates → **images in Artifact Registry (+ Image
Streaming if a task reads a fraction)**, **`pipelined`** to overlap remaining pull, and
**`epochs`/`keep_warm` with `IfNotPresent`** so passes after the first are nearly
pull-free. If the set fits node disk and the run spreads across the pool, **`prepull()`**
up front warms every node before the first claim. Grow `window_size` (with
`avg_image_gb`/`node_ephemeral_gb`/`cluster_nodes` as guardrails) once disk allows.
`warm_per_task` doesn't apply.

**RL (G rollouts per problem).** Depth and locality beat overlap → **`warm_per_task=True`**
(raise `max_warmpool_size` ≥ G) for one ready replica per rollout, **`colocate_replicas=True`**
so only the first pulls, **`naive` or `sliding`** (*not* `pipelined`), and **`keep_warm=True`**
across training steps. This optimizes the per-rollout claim tail, not batch wall.
