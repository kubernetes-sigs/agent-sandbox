<!-- Redacted sample report (environment scrubbed). Regenerate real reports with tests/loadtest.py; they are gitignored. -->

# SWE-bench load test

## Parameters

- **images**: `10`
- **tasks_per_image**: `8`
- **total_tasks**: `80`
- **strategies**: `naive`
- **max_concurrent**: `40`
- **max_warmpool_size**: `8`
- **warm_per_task**: `True`
- **colocate_replicas**: `True`
- **window_size**: `None`
- **task_duration_s**: `15.0`
- **image_template**: `REGISTRY/PROJECT/swebench-mirror/bench:img{i:03d}`

## Methodology

Each strategy runs the **same** synthetic SWE-bench batch — **10** distinct container images, **8** task(s) per image (**80** tasks total) — end to end against the live cluster, then tears its pools down before the next strategy starts (so strategies never share warm state).

Per task the harness: (1) ensures a `SandboxTemplate` + `SandboxWarmPool` exist for the task's image — *when/how many* pools are pre-warmed is what the **strategy** decides; (2) **claims** a ready sandbox; (3) runs `process_fn` — here a probe that sleeps `task_duration=15.0s`; (4) releases the sandbox.

Wall-clock is the true end-to-end batch time under `max_concurrent=40`. Per-phase totals are summed across concurrent workers, so they exceed wall-clock — divide by the phase count (`n`) for the average a single task saw. **Efficiency** = fastest strategy's wall ÷ this strategy's wall.

RL instant-claim mode: **per-task sizing** (one warm replica per task — `min(tasks_image, max_warmpool_size=8)` — so claims are near-instant); **replica co-location** (a pool's replicas prefer one node, so only the first pulls the image and the rest start from the node layer cache).

Strategies compared:
- **naive** — all pools warmed up front (peak = #images) — fastest claims, largest footprint.

## Cluster & nodes

- `loadtest`: **context**=gke_<project>_<region>_<cluster>  **namespace**=agent-sandbox-rl  **k8s_version**=v1.3x.x-gke.xxxx  **node_pools**=[<node-pool>]  **instance_types**=[e2-standard-16]  **region**=<region>

## Warm-pool plan (10 pools)

| image | pool | replicas | cluster |
|---|---|---:|---|
| `bench:img000` | `pool-r2e-img-1afdd43e943c` | 8 | loadtest |
| `bench:img001` | `pool-r2e-img-84e70362d8ea` | 8 | loadtest |
| `bench:img002` | `pool-r2e-img-f32f0a9c4d96` | 8 | loadtest |
| `bench:img003` | `pool-r2e-img-ef990e5b7f14` | 8 | loadtest |
| `bench:img004` | `pool-r2e-img-aaa007687338` | 8 | loadtest |
| `bench:img005` | `pool-r2e-img-499c653fff24` | 8 | loadtest |
| `bench:img006` | `pool-r2e-img-492cda9a7643` | 8 | loadtest |
| `bench:img007` | `pool-r2e-img-1dea5bc381df` | 8 | loadtest |
| `bench:img008` | `pool-r2e-img-d0d187b45a8c` | 8 | loadtest |
| `bench:img009` | `pool-r2e-img-215cfcaa3401` | 8 | loadtest |

## Results — per stage, per strategy

> **wall** is the true end-to-end time for the whole batch. Per-phase columns are **summed across concurrency**, so they exceed the wall-clock — the `avg` figures are the per-task experience. **claim avg/max** is the *time-to-sandbox* (request → ready, claimed sandbox); **efficiency** = fastest strategy's wall ÷ this strategy's wall.

| strategy | wall | prep (create+wait) | pool-ready avg/max | claim avg/max | claims | net task Σ/avg | warm pools (peak/total/created) | ok | efficiency |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| **naive** | 54.0s | 86.8s | 8.1/9s | 2.6/6s | 80 | 1200.1/15.00s | 80/80/10 | 80/80 | 100% |

## Metric glossary

Each line in a strategy's RunReport reads `phase  total_s  (n=count, max=slowest)`:

| field | meaning |
|---|---|
| `total_s` | wall-time **summed across all concurrent workers** spent in this phase |
| `n` | how many times the phase ran (one per pool, or one per task) |
| `max` | the slowest single occurrence — the tail latency for that phase |

| phase | what it measures |
|---|---|
| `preflight` | one-time cluster checks (API reachable, namespace, CRDs) before any work |
| `plan` | compute the image→warm-pool plan (in-memory; ~0s) |
| `create_warmpool` | issue the `SandboxWarmPool` create calls (n = pools created) |
| `wait_pool_ready` | block until a pool replica is Ready — the **image pull + pod start** cost |
| `prefetch` | *(pipelined only)* background warm of the next window, overlapped with execution |
| `claim` | request a sandbox → have a claimed, ready one — **time-to-sandbox** |
| `process` | time inside `process_fn` (the task itself; 0s here by design) |
| `release` | return the sandbox / claim to the pool |
| `teardown` | delete pools + templates at the end of the run |

## Full RunReport per strategy

### naive
```
  preflight            1.25s  (n=1, max=1.25s)
  plan                 0.00s  (n=1, max=0.00s)
  create_warmpool      6.23s  (n=10, max=0.70s)
  wait_pool_ready     80.54s  (n=10, max=9.43s)
  claim              210.92s  (n=80, max=5.96s)
  process           1200.09s  (n=80, max=15.00s)
  release             12.63s  (n=80, max=0.34s)
  teardown             3.56s  (n=1, max=3.56s)
  TOTAL wall          54.05s
  claims=80  tasks=80ok/0err  warm peak=80
```

## Image list

```
REGISTRY/PROJECT/swebench-mirror/bench:img000
REGISTRY/PROJECT/swebench-mirror/bench:img001
REGISTRY/PROJECT/swebench-mirror/bench:img002
REGISTRY/PROJECT/swebench-mirror/bench:img003
REGISTRY/PROJECT/swebench-mirror/bench:img004
REGISTRY/PROJECT/swebench-mirror/bench:img005
REGISTRY/PROJECT/swebench-mirror/bench:img006
REGISTRY/PROJECT/swebench-mirror/bench:img007
REGISTRY/PROJECT/swebench-mirror/bench:img008
REGISTRY/PROJECT/swebench-mirror/bench:img009
```