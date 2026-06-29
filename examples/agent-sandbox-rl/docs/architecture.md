# Architecture & lifecycle

`agent-sandbox-rl` is a thin orchestration layer over the `k8s-agent-sandbox`
SDK. It owns the *batch / fleet* concerns the SDK doesn't: resource CRUD,
sizing, placement across clusters, strategies, preflight, pre-pull, and parallel
execution.

## Layers

```
            your RL framework / CLI
                     │
        SandboxFleet / AsyncSandboxFleet        (orchestrator)
        ├── sources      Task / *Source         (what to run)
        ├── placement    image → Cluster        (where)
        ├── sizing       replicas per image      (how many; concurrency/disk-aware)
        ├── strategies   none/naive/sliding/pipelined  (when pools exist)
        ├── capacity     probe → benchmark plan  (strategy/concurrency/replicas)
        ├── registry_rewrite  image → mirror     (in-region pull-through)
        ├── preflight    per-cluster checks
        ├── prepull      DaemonSet image cache
        └── ClusterRegistry → Cluster(s)
                              ├── Resources      Template/WarmPool CRUD (v1beta1)
                              └── k8s_agent_sandbox SDK   (claim/exec/terminate)
                     │
              Agent Sandbox controller (CRDs) on each cluster
```

Only `Resources` (SandboxTemplate/SandboxWarmPool CRUD) and the per-cluster
wiring are genuinely new k8s code; **claiming reuses the SDK** (`SandboxClient`,
`K8sHelper`) — one set per cluster, pointed at its context by attribute
injection (no SDK fork).

## Components

- **`config.py`** — `FleetConfig`, `ClusterConfig`, `TemplateSpec`,
  `ResourceSpec` (pydantic). `FleetConfig.template_name(image)` → `r2e-img-<md5>`.
- **`cluster.py`** — `Cluster` (own `ApiClient` per context →
  `CustomObjectsApi`/`CoreV1Api`/`AppsV1Api` + `Resources`; lazy injected
  `K8sHelper`/`SandboxClient`; placement/capacity bookkeeping) and
  `ClusterRegistry`.
- **`resources.py`** — `Resources`: `ensure_template`, `create_warmpool`,
  `wait_for_pool_ready` (Kubernetes **watch** on the WarmPool — readiness detected
  at the `readyReplicas` event, no fixed poll grid; reconnects + short re-check
  fallback on drops), `delete_*`, `list_*` (label-scoped). The missing SDK piece.
- **`sizing.py`** — `compute_replicas` (concurrency-aware, `per_task` mode for
  instant-claim), `recommend_window`, `recommend_window_disk` /
  `recommend_window_pipelined` (disk- and pool-node-aware window caps), `plan`.
- **`capacity.py`** — `probe_capacity` (a pool's allocatable CPU / ephemeral disk /
  pod density), `plan_benchmark` (`naive` warm-all when it fits, else a disk-bounded
  `pipelined` window; picks `max_concurrent`, per-image replicas, the binding
  bottleneck, and the RL instant-claim levers), `render_plan`.
- **`registry_rewrite.py`** — `rewrite_image` / `make_rewriter`: redirect images at
  an in-region mirror / pull-through cache (Docker Hub → `<registry>/…`), host-
  filtered + idempotent; wired via `load_tasks(image_rewrite=…)`.
- **`sources.py`** — `Task`, `TaskSource`, `ListSource`, `JsonlSource`,
  `to_tasks`. **`adapters/swebench.py`** — `SweBenchSource` (+ `keep_row` for the
  full dataset row) + `swebench_probe`. **`adapters/r2egym.py`** —
  `make_fleet_repo_env`/`FleetRepoEnv`/`FleetDockerRuntime`: subclass R2E-Gym's
  `RepoEnv`/`DockerRuntime` to **bind** a fleet-warmed pod (override
  `_start_kubernetes_sandbox` to read the handle's pod via a per-runtime
  `CoreV1Api`, `_stop_kubernetes_sandbox` to a no-op so the fleet owns the pod,
  and exec/file-copy to forward the handle's namespace). Lazy/guarded so the core
  imports without R2E-Gym; serves tunix deepswe transitively.
- **`placement.py`** — `RoundRobin`, `LeastLoaded`, `CapacityWeighted`,
  `ImageAffinity` (capacity-aware) + `get_placement`.
- **`handles.py`** — `SandboxHandle` (`hostname`, `pod_name`, `pod_ip`,
  `endpoint`, `exec`, `release`); `exec` builds a fresh `ApiClient` per call
  (thread-safe).
- **`preflight.py`** — `preflight_cluster` → `PreflightReport`.
- **`prepull.py`** — DaemonSet pre-pull (`prepull` / `prepull_delete`).
- **`fleet.py`** — `SandboxFleet` (incl. `epochs`/`keep_warm` reuse, parallel
  windowed warming, disk-aware `recommended_window`); **`strategies.py`** —
  `process_parallel` + the four strategies (`none`/`naive`/`sliding`/`pipelined`);
  **`async_fleet.py`** — `AsyncSandboxFleet`.
- **`observability.py`** — `Observer` (the sink the fleet holds), `RunReport`
  (always-on per-phase aggregates), `repo_family` (label-cardinality bound),
  `serve_metrics`. Each fleet phase is wrapped in `observer.phase(...)`, which
  times the block, observes the Prometheus `asrl_*` histogram, folds the
  duration into the current `RunReport` (lock-guarded for parallel workers), and
  — when tracing is on — opens an `asrl.<phase>` span. The per-cluster
  `SandboxClient` gets our `trace_service_name` so its claim/exec spans share the
  one provider and nest under the fleet spans. All Prometheus/OTel work is
  guarded by the `ObservabilityConfig` flags, so disabled layers are no-ops while
  `RunReport` stays always-on. `run()` also attaches per-cluster details
  (`SandboxFleet.describe_environment()`) to `report.environment`; the report's
  `summary()`/`to_dict()` render it, and `examples/run_swebench_fleet.py` persists
  timestamped reports to `REPORT_DIR` (see
  [`../performance_reports/README.md`](../performance_reports/README.md)).

## Lifecycle

```
load_tasks ─▶ preflight ─▶ plan ─▶ [prepull] ─▶ start_warmpools ─▶ acquire* ─▶ release* ─▶ teardown
                                   (per-cluster)   (sized pools)    (claim+    (delete    (sweep claims→
                                                                     hostname)  claim)     pools→templates)
```

- **plan**: each unique image → a cluster (placement); replicas sized per
  `(cluster, image)` via `sizing`.
- **acquire**: SDK `create_sandbox(warmpool=…)` → resolve sandbox → `get_pod_name`
  → `SandboxHandle`. Claims are labeled for sweepable cleanup.
- **strategies**: `naive` warms all then runs; `sliding` warms a rolling window;
  `pipelined` double-buffers the window (prefetch N+1 while N runs, footprint ≤ 2
  windows); `none` warms one size-1 pool per image. Windowed strategies warm a
  window's pools concurrently (bounded by `max_concurrent`). All run claim+exec in
  parallel up to `concurrency` (threads sync, `asyncio.gather` async). `run(...,
  epochs=N, keep_warm=…)` repeats passes, reusing resident pools (node-cache hits
  via `imagePullPolicy: IfNotPresent`) and tearing down once at the end.

## SDK reuse (no fork)

agent-sandbox-rl **reuses the `k8s-agent-sandbox` SDK as-is** — it never forks or
vendors it. The only deltas are *injected at runtime*:

- **Per-cluster client by attribute injection.** The SDK's
  `SandboxClient.__init__` builds a default `K8sHelper()` bound to the ambient
  kube context — fine for one cluster, wrong for many. So each `Cluster` builds
  its own `ApiClient` via `new_client_from_config(context=…)` and a `K8sHelper`
  whose `custom_objects_api` / `core_v1_api` point at it, then swaps that helper
  onto a fresh `SandboxClient` (`cluster.py` `sandbox_client`):

  ```python
  c = SandboxClient(tracer_config=self.tracer_config)  # or SandboxClient()
  c.k8s_helper = self.k8s_helper        # context-scoped helper, injected
  ```

  Result: one upstream `SandboxClient` *instance per cluster*, all running the
  same SDK code, no fork. (`tracer_config` is the other injection — it makes the
  SDK emit its `create_claim` / `wait_for_sandbox_ready` spans into the same OTel
  provider as our fleet spans; see Observability.)
- **We use the SDK for the claim lifecycle only** — `create_sandbox(...)`,
  `delete_sandbox(...)`, `terminate()`. We deliberately **bypass** the SDK's
  `.commands` / `.files` (which require the Sandbox Router) and run commands
  router-free via the pod exec API (`handle.exec`).

A native `K8sHelper(api_client=…)` parameter would let us drop the attribute
swap — a candidate upstream improvement, not a blocker.

### acquire → ready → work → release

What happens between "RL framework asks for a sandbox" and "it starts working":

```
fleet.acquire(task)                                  # agent_sandbox_rl
  ├─ plan lookup: image → (cluster, warmpool)
  └─ cluster.sandbox_client.create_sandbox(          # SDK (sandbox_client.py)
         warmpool, namespace, ready_timeout, labels)
        ├─ _create_claim()          POST SandboxClaim CR → binds a warm pod
        ├─ resolve_sandbox_name()   poll claim.status → concrete Sandbox name
        └─ _wait_for_sandbox_ready() poll Sandbox CR → status=Ready
      returns SDK Sandbox (claim_name, sandbox_id, get_pod_name/get_pod_ip)
  └─ wrap → SandboxHandle  ◀── returned to the RL framework HERE
```

The framework gets control back the moment `create_sandbox` returns — i.e. once
the claim is **bound to a warm pod** *and* the Sandbox reports **Ready**. From
that `SandboxHandle` it works in one of two ways, then releases:

```python
h = fleet.acquire(task)
try:
    h.exec(["sh", "-c", "cd /testbed && python -m pytest -q"])  # (a) router-free exec
    # or, to talk to a server inside the sandbox:
    url = h.endpoint(8888)        # (b) "<sandbox_id>.<namespace>:8888"
finally:
    fleet.release(h)             # SDK terminate() → deletes the claim
```

There is **no SSH/login step**: `exec` is the Kubernetes pod-exec API straight to
`pod_name`, and `endpoint(port)` is the sandbox's stable in-cluster DNS name.

**Pod reuse caveat.** A claim does *not* hand back a fresh pod — it binds you to
an already-running warm pod from the pool (that's why claims are sub-second
instead of an image-pull). This fleet does **one claim per task** and deletes the
claim on `release` (the controller then reaps/replaces the pod); the next task
binds a *different* warm replica. Warm pods are recycled across the pool's
lifetime by the controller, never handed dirty from one task to the next by us.

## Connection model

A Sandbox has a **stable in-cluster DNS name** = `sandbox_id` = `handle.hostname`.
In-cluster learners connect via `handle.endpoint(port)` (`<hostname>.<ns>:<port>`)
or run commands with `handle.exec` (router-free, via the pod exec API — the SDK
Sandbox Router is *not* required). Out-of-cluster / cross-cluster learners need
per-cluster routable endpoints (Gateway/LoadBalancer).

## Design notes

- **Multi-cluster via attribute injection.** Each `Cluster` builds its own
  `ApiClient` (`new_client_from_config(context=…)`) and points the SDK's
  `K8sHelper`/`SandboxClient` at it. No SDK changes required; a native
  `K8sHelper(api_client=…)` param is a candidate upstream improvement.
- **Async is a thread-backed wrapper.** `AsyncSandboxFleet` reuses the tested
  sync core via a **dedicated `ThreadPoolExecutor`** (sized to `max_concurrent`,
  not `asyncio.to_thread`'s shared default pool — which is tied to CPU count and
  starved/deadlocked `pipelined` at scale) with real concurrency
  (`gather` + `Semaphore`); `close()`/`__aexit__` shut the pool down. The API is
  fully awaitable; a native `kubernetes_asyncio` backend can replace the internals
  later.
- **Thread-safety.** Fleet bookkeeping is lock-guarded; `handle.exec` uses a
  per-call `ApiClient` because kubernetes `stream()` (websocket) isn't safe
  across a shared client.
- **Cleanup safety.** Everything is labeled `app=agent-sandbox-rl`; teardown
  sweeps stray claims before pools/templates so a leaked claim can't keep its
  adopted sandbox alive.
- **Two sizing modes for two workloads.** The default `compute_replicas` sizes each
  pool to its *share of the concurrency budget* — optimal for **eval** (a 1:1 sweep
  where each image has one task). `FleetConfig.warm_per_task` switches to
  *one replica per task* (`min(tasks_image, max_warmpool_size)`) for **RL**, where the
  same problem image is claimed by *G* rollouts at once. `TemplateSpec.colocate_replicas`
  then keeps those *G* replicas on one node (soft `podAffinity` on the shared
  `sandbox=<template>` label) so only the first pulls the image. Both default off and
  compose with any strategy; see [eval vs RL](../README.md#eval-vs-rl--recommended-recipes).

## Optimization findings (from the rl-sandbox-scripts example)

The strategies/sizing/prepull here encode measured results from the companion
`rl-sandbox-scripts` prototype (a separate contribution) — its `optimizations.md`,
`performance.md`, and `image-analysis.md` working notes record: warm-pool claims
are sub-second; image **layer sharing** makes pre-pull pay off per repo-family;
concurrency-aware sizing slashes idle footprint; parallel claim+exec scales the
task region ~linearly; the SWE-Bench-Verified set is 500 images / 12 families
(django ≈ 46%).

Since image pull dominates wall-clock at scale, the `pipelined` strategy overlaps
the next window's pull with the current window's execution, and `epochs`/`keep_warm`
+ `imagePullPolicy: IfNotPresent` amortize pulls across passes via the node layer
cache. Every strategy, sizing mode, and caching/infra lever (in-region Artifact
Registry mirror via the `image_rewrite` hook, pre-pull DaemonSet, Image Streaming,
larger/secondary boot disk, disk-aware window sizing) — with the workload each fits
and the exact flag — is documented in [`strategies.md`](strategies.md).

A later load-test sweep (`tests/loadtest.py`, recorded under `performance_reports/`)
added the **instant-claim** findings: `warm_per_task` + `colocate_replicas` cut the
per-rollout claim *tail* (e.g. 10 s → 3 s) but not batch wall — wall is bounded by
`max_concurrent`, which the default sizing already saturates, so the benefit is
straggler latency in synchronous RL steps, not throughput. One caveat the sweep
surfaced: combining `warm_per_task` with `pipelined` shrinks the prefetch window
(deeper per-image footprint ⇒ fewer images per window), serializing problems and
underfilling concurrency once rollouts do real work — so RL should pair
`warm_per_task` with `naive`/`sliding`, and reserve `pipelined` for pull-bound 1:1
eval. (Follow-up: pipelined window sizing should weigh concurrency utilization, not
just footprint.)
