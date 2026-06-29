# Performance tuning on GKE

At scale, wall-clock for SWE-bench-style fleets is dominated by **image pull**
(`wait_pool_ready` in the RunReport), not task work. A full SWE-bench-Verified run
(500 unique multi-GB images) is ~99 % pull. This guide covers the levers — most
are node-pool / registry settings that live *outside* this package (it stays
infra-agnostic), plus the small in-package knobs that target them.

## All levers at a glance

Two independent axes plus a set of caching levers. Pick a **warm-pool strategy**
(*when/how many* pools exist) and a **sizing mode** (*how deep* each pool is), then
stack the caching levers that fit your cluster.

| Lever | What it does | Knob / API | Best for |
| :--- | :--- | :--- | :--- |
| **Warm-pool strategy** | when pools exist & footprint | `run(strategy=…)`: `naive` / `sliding` / `pipelined` / `none` | `pipelined` for pull-bound eval; `naive`/`sliding` for RL |
| **Concurrency-aware sizing** (default) | pool depth = image's share of the budget | `max_concurrent`, `max_warmpool_size` | eval (1:1), minimal footprint |
| **Instant-claim (pre-warm)** | one warm replica **per task** | `FleetConfig.warm_per_task=True` | RL (G rollouts share an image) |
| **Replica co-location** | pack a pool's replicas on one node → 1 pull/node | `TemplateSpec.colocate_replicas=True` | deep pools (RL), cache reuse |
| **Cross-epoch reuse** | keep pools resident across passes | `run(epochs=N)` / `keep_warm=True` | repeated passes (RL epochs) |
| **Node layer cache** | re-use already-pulled layers | `TemplateSpec.image_pull_policy=IfNotPresent` (default) | every repeated run |
| **Pre-pull DaemonSet** | cache images on **every** node *before* the hot path | `fleet.prepull()` / `setup(prepull=True)` | spreading a fixed set across many nodes / autoscale |
| **In-region mirror** | cut cross-region pull + Docker Hub rate limits | `make_rewriter(...)` + `load_tasks(image_rewrite=…)` | Docker Hub-hosted images |
| **Image Streaming** | pods Ready before the full pull | node-pool `--enable-image-streaming` + `node_selector` | large images, task reads a fraction |
| **Disk-aware window** | cap the auto window to node disk | `avg_image_gb` / `node_ephemeral_gb` / `cluster_nodes` | growing the window safely |
| **Bigger / secondary boot disk** | more resident layers / images present at boot | node-pool setting + `node_selector` | fixed image sets, autoscale-warm |

The rest of this guide details each, roughly in order of impact for repeated (RL) runs.

## 1. Amortize pulls across epochs (in-package)

Re-pulling every epoch is the biggest avoidable cost. Two mechanisms:

- **Node layer cache** — `TemplateSpec.image_pull_policy` defaults to
  `IfNotPresent`, so once an image's layers are on a node, re-creating its pool
  (next window or next epoch) skips the pull. Set `Always` only if a tag mutates.
- **`epochs=N` / `keep_warm=True`** — `fleet.run(..., epochs=N)` keeps pools
  resident between passes and tears down once at the end; `keep_warm=True` leaves
  them up for a caller-driven loop. Epoch 2+ then hits the node cache and
  `wait_pool_ready` collapses toward the claim+process floor.
- **Replica co-location** — `TemplateSpec.colocate_replicas=True` adds a soft
  pod-affinity (`topologyKey: kubernetes.io/hostname`) on the shared
  `sandbox=<template>` label, so a pool's replicas prefer the *same* node. With
  `IfNotPresent`, only the **first** replica of a pool pulls the image; the rest
  start from that node's layer cache — turning an *N*-replica pool into one pull
  instead of *N*. This is the within-pool analogue of the cross-epoch cache, and
  it pairs naturally with `warm_per_task` (deep pools for instant claims). The
  affinity is *preferred*, not *required*, so it spills to other nodes (each
  re-pulling once) rather than dead-locking when a node is full. Budget node
  capacity for `replicas × cpu_request` (e.g. 50 × 250m ≈ 13 vCPU → an
  `e2-standard-16`).

## 2. Pre-pull the working set onto nodes (in-package)

`epochs`/`keep_warm` warm the node cache *as a side effect of the first pass*;
pre-pull warms it **up front and on every node**, before any claim. `fleet.prepull()`
(or `setup(prepull=True)`) lays down a short-lived **DaemonSet** with one init
container per image (each `IfNotPresent`, `sh -c exit 0`), so every selected node
pulls + unpacks every image into its local containerd cache once; it waits until all
nodes report ready, then `prepull_delete()` removes it.

```python
fleet.load_tasks(SweBenchSource(limit=500))
fleet.plan()
fleet.prepull()            # every node caches all 500 images; or setup(prepull=True)
fleet.run(process_fn, strategy="sliding")   # warm pools now hit the node cache
```

- It honors `TemplateSpec.node_selector` / `image_pull_secret` and runs **per
  cluster**, and (being a DaemonSet) **auto-covers newly-autoscaled nodes**.
- It's the only mechanism that spreads ready images across **all** nodes ahead of
  the hot path — unlike `colocate_replicas` (one node per pool) or `epochs` (only
  the nodes a pass happened to schedule on). Best when the working set fits node
  disk and the run is scheduling-spread or scales up mid-run.
- Bounded by node disk — pair with the disk-aware window (§5) for large sets.

## 3. In-region registry / pull-through cache

Cross-region pulls from Docker Hub (and its rate limits) are slow. Mirror the
images into a registry in your cluster's region and point the fleet at it:

- **Artifact Registry remote (pull-through) repo** caches Docker Hub in-region on
  first pull — no eager copy:
  ```bash
  gcloud artifacts repositories create dockerhub-cache \
    --repository-format=docker --mode=remote-repository \
    --remote-docker-repo=DOCKER-HUB --location=us-central2
  ```
- **Or a standard repo + an eager copy** (full Docker-Hub independence /
  reproducibility) via Cloud Build registry-to-registry `crane copy`.
- Redirect tasks with the built-in rewriter (no change to the task source):
  ```python
  from agent_sandbox_rl import make_rewriter
  fleet.load_tasks(source, image_rewrite=make_rewriter(
      registry="us-docker.pkg.dev", project="PROJECT", repo="REPO"))
  ```
  Grant the node service account `roles/artifactregistry.reader` on the repo.

## 4. GKE Image Streaming (gcfs)

With images in Artifact Registry, [Image
Streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
lets pods become **Ready before the full image is local** — containerd streams
layer bytes on demand. For large images where a task touches a fraction of the
bytes, this can turn the cold-pull tail (the worst-case `wait_pool_ready`) into
seconds. It's a node-pool setting (`--enable-image-streaming`); target a
streaming-enabled pool with `TemplateSpec.node_selector` (see §6).

## 5. Bigger / faster node disk (to grow the window)

More images resident per node ⇒ a larger `window_size` ⇒ fewer windows ⇒ less
window-barrier overhead. Disk is the limit:

- A larger boot disk (or `pd-ssd` / local SSD) holds more layers, and on
  `pd-balanced`/`pd-ssd` IOPS scales with size, which also speeds the
  decompress-bound pull.
- **Secondary boot disk** lets you bake images into a disk image once and attach
  it to the pool, so they're present at node boot (zero pull on a fresh node).
- Tell the sizer about disk so a bigger window can't over-fill nodes:
  ```python
  FleetConfig(avg_image_gb=3.8, node_ephemeral_gb=350, disk_headroom=0.25)
  ```
  The auto window for `sliding`/`pipelined` is then capped to fit usable disk.

## 6. Targeting a node pool

Image Streaming, secondary boot disk, and larger disks are all node-pool
properties. Target the right pool per image via the existing template knobs — no
package-specific code:

```python
FleetConfig(template=TemplateSpec(
    node_selector={"cloud.google.com/gke-nodepool": "streaming-ssd-pool"},
    extra_pod_spec={"tolerations": [{"key": "dedicated", "operator": "Exists"}]},
))
```

## Putting it together

The right stack depends on the workload — eval is *pull-bound*, RL is *claim-bound*
per problem (see [eval vs RL](../README.md#eval-vs-rl--recommended-recipes)).

**Eval (1:1 SWE-bench sweep — many images, one task each).** Pull dominates, so the
highest-leverage stack is: **images in Artifact Registry + Image Streaming**,
**`pipelined`** to overlap whatever pull remains, and **`epochs`/`keep_warm` with
`IfNotPresent`** so passes after the first are nearly pull-free. If the set fits node
disk and the run spreads across the pool, **`prepull()`** up front warms every node's
cache before the first claim (and covers autoscaled nodes). Grow `window_size`
(with `avg_image_gb`/`node_ephemeral_gb` as a guardrail) only once disk allows.
`warm_per_task` doesn't apply (one task per image → one replica).

**RL (G rollouts per problem).** The same problem image is claimed *G* times at once,
so depth and locality matter more than overlap: **`warm_per_task=True`** (raise
`max_warmpool_size` to ≥ G) gives every rollout its own ready replica, and
**`colocate_replicas=True`** packs those replicas on one node so only the first pulls
the image. Use **`naive` or `sliding`**, *not* `pipelined` — with deep per-problem
replicas the pipelined window shrinks and serializes problems, underfilling
`max_concurrent` once rollouts do real work. Keep pools resident across training steps
with **`keep_warm=True`**. This optimizes the per-rollout claim tail (the synchronous
straggler), not batch wall.
