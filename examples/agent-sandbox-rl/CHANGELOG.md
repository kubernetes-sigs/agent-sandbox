# Changelog

All notable changes to `agent-sandbox-rl`. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versions are dev-stage.

## [0.1.0.dev0] — unreleased

Initial implementation (design phases 1–7), live-verified on GKE against Agent
Sandbox `v0.5.0rc1` (v1beta1).

### Added (performance & scale)
- **`pipelined` strategy** (`strategies.py`, `async_fleet.py`): double-buffered
  sliding window — prefetch window N+1's pools (background thread / `asyncio.Task`)
  while window N's tasks run, so image pull overlaps execution. Footprint bounded
  ≤ 2 windows; new `prefetch` phase in the `RunReport`.
- **Cross-epoch warm cache** (`fleet.py`, `async_fleet.py`): `run(..., epochs=N)`
  runs N passes keeping pools resident between them (re-pulls hit the node layer
  cache), tearing down once at the end (returns `list[list]`); `keep_warm=True`
  leaves pools up for a caller-driven loop. `warm_image`/`start_warmpools` gained a
  reuse guard so reuse never re-creates or double-reserves.
- **Async fleet dedicated thread pool** (`async_fleet.py`): `AsyncSandboxFleet` runs
  all blocking k8s calls on its own `ThreadPoolExecutor` sized to `max_concurrent`
  (`min(1024, max(64, max_concurrent + 2×window + 16))`) instead of
  `asyncio.to_thread`'s shared default pool (`min(32, cpu+4)`). The default pool is
  tied to the driver's CPU count, not concurrency: under `pipelined` (which overlaps
  prefetch + process + unwarm, and `wait_for_pool_ready` holds a thread) it starved
  teardown and **deadlocked** at scale. Fixes that; live-validated (100/100 where it
  previously hung). `close()` shuts the pool down (called from `__aexit__`).
- **Parallel pool warming, all strategies** (`fleet.py`, `strategies.py`):
  `start_warmpools` and the **windowed strategies** (`sliding`/`pipelined`) now warm
  pools **concurrently** (bounded by `max_concurrent`). Extracted the fan-out into
  `fleet._warm_entries` and added `fleet.warm_images(images)` (a window warmed in
  parallel); `_run_windowed`/`_run_pipelined` use it instead of warming one image at a
  time. Measured at 500 images: `sliding` dropped **2556s → 279s (9.2×)**, making the
  windowed strategies competitive with `naive` (the async path was already concurrent).
- **Node-aware disk window sizing** (`sizing.py`, `config.py`, `fleet.py`):
  `recommend_window_disk` / `recommend_window_pipelined` gained a `nodes` arg so the
  disk budget is the **whole pool's** usable disk (distinct images spread across nodes),
  not a single node's. New `FleetConfig.cluster_nodes` feeds it (None = conservative
  single-node bound). On a 30-node pool this lifted the auto window from 25 → 500 images;
  the capacity planner sets it from the probed node count.
- **Instant-claim mode for RL** (`config.py`, `sizing.py`, `fleet.py`,
  `resources.py`): two opt-in levers (both default off) that trade resources for
  near-zero claim latency. **`FleetConfig.warm_per_task`** sizes each pool to
  `min(tasks_image, max_warmpool_size)` (one warm replica per task), so every task
  claims immediately (`compute_replicas(..., per_task=True)`, threaded through the
  window/disk sizing so windows shrink for the deeper pools; warns+clamps when an
  image has more tasks than `max_warmpool_size`). **`TemplateSpec.colocate_replicas`**
  adds a soft `podAffinity` (`topologyKey: kubernetes.io/hostname` on the shared
  `sandbox=<template>` label) so a pool's replicas prefer one node — only the first
  pulls the image, the rest start from the node layer cache. These target **RL**
  (G rollouts per problem image); for **1:1 eval** they're no-ops. They cut the
  per-rollout claim *tail* (the synchronous straggler), not batch wall. Pair with
  `naive`/`sliding`, **not** `pipelined` (deep replicas shrink its window and
  serialize problems). See the README "Eval vs RL" recipes.
- **`TemplateSpec.image_pull_policy`** (`config.py`, `resources.py`): default
  `IfNotPresent` so the node layer cache is reused across runs/epochs.
- **Disk-aware window sizing** (`sizing.py`, `config.py`, `fleet.py`):
  `recommend_window_disk` / `recommend_window_pipelined`, with
  `FleetConfig.avg_image_gb` / `node_ephemeral_gb` / `disk_headroom`; caps the auto
  window so resident images fit node disk.
- **Image rewriting** (`registry_rewrite.py`): `rewrite_image` / `make_rewriter`
  + a `load_tasks(image_rewrite=...)` hook to redirect images at an in-region
  mirror / pull-through cache (original kept in `metadata['original_image']`).
- **Load-test harness** (`tests/loadtest.py`): parameterized SWE-bench-style load
  test — `--images N --tasks-per-image K --strategies all|… --image-template …
  [--task-duration S]` against a live cluster — emitting a self-explanatory report:
  a **methodology** section, cluster/nodes, image list, warm-pool plan, a per-stage
  benchmark per strategy (prep create+wait, pool-ready avg/max, **claim latency
  avg/max = time-to-sandbox**, claims, net task time, warm-pool peak/total/created,
  wall, efficiency), and a **metric glossary** for the raw RunReport phases. Pure
  helpers unit-tested in `tests/test_loadtest.py`.
- **Capacity-aware preload planner** (`agent_sandbox_rl/capacity.py`): a first-class,
  importable API — `probe_capacity` (reads a pool's allocatable CPU + ephemeral storage +
  pod density), `plan_benchmark` (optimal plan: strategy `naive` warm-all when it fits, else
  a disk-bounded `pipelined` window; `max_concurrent`; per-image replicas; binding
  bottleneck cpu/disk/pods), and `render_plan` (no extra deps). Exposed via
  `from agent_sandbox_rl import probe_capacity, plan_benchmark, render_plan`. Three entry
  points: the API, an **interactive wizard** (`examples/plan_capacity.py` — prompts for
  cluster/pool/batch, prints the plan, offers to run; plan-only/read-only by default), and
  the benchmark CLI (`tests/run_full_swebench_benchmark.py`, plan-only by default;
  `--execute` runs it and reports **preload vs task** wall separately). Pure helpers
  unit-tested in `tests/test_capacity.py`.
- **Docs**: `docs/strategies.md` — consolidated strategies / sizing / tuning guide
  (decision table, all-levers table, the four strategies, instant-claim sizing,
  caching levers incl. pre-pull, Image Streaming, AR mirror, secondary boot disk);
  README `pipelined`/`epochs`/`keep_warm` + Performance tuning section.

### Fixed (from PR #1049 review)
- **Async `close()` blocks + cancels by default** (`async_fleet.py`): explicit
  `close()` / `__aexit__` now `shutdown(wait=True, cancel_futures=True)` so no
  non-daemon worker thread outlives the close; `__del__` keeps `wait=False` to
  avoid hanging during GC/finalization.
- **Explicit empty registry honored** (`fleet.py`): `SandboxFleet(..., registry=ClusterRegistry([]))`
  no longer falls back to a default ambient `Cluster` (which loads kube-config and
  failed CI where none exists). `ClusterRegistry` defines `__len__`, so the old
  `registry or default` treated an empty registry as falsy — now `is not None`.
  This was the failing `presubmit-agent-sandbox-unit-test` (two `test_registry_rewrite`
  tests passed locally only because a kube-config was present).
- **Pipelined peak-pod estimate** (`capacity.py`): `plan.total_warm_pods` for the
  pipelined branch now reports `2 × window × replicas` (double-buffered: current +
  prefetch window), matching the x2 per-node disk estimate, instead of one window.
- **`_warm_entry` honors `wait=True` on reuse** (`fleet.py`): an already-warm image
  no longer skips the readiness wait when re-warmed with `wait=True` (a prior warm
  may have used `wait=False`), so the "optionally wait for readiness" contract holds.
- **`plan_benchmark` validates numeric args** (`capacity.py`): raises `ValueError`
  for `cpu_request_milli < 1` (was `ZeroDivisionError`), `max_pool < 1`,
  `avg_image_gb <= 0`, and `disk_headroom` outside `[0, 1)`.
- **`_disk_spec` docstring corrected** (`fleet.py`): documents that `usable` is
  `None` (disk cap disabled) unless *both* disk hints are set.
- **Epoch teardown on failure** (`fleet.py`, `async_fleet.py`): a non-final
  `epochs>1` pass that raised left warm pools/claims resident (only the last epoch
  carried `teardown=True`). Both fleets now tear down on a mid-run epoch error when
  `keep_warm=False`.
- **Warm-pool replica reconcile + delta reserve** (`resources.py`, `fleet.py`):
  `create_warmpool(..., reconcile=True)` upserts `spec.replicas` on 409 so a reused
  pool that needs *more* replicas converges instead of being pinned at its old size
  (which hung `wait_for_pool_ready`); `_warm_entry` reserves only the delta so reuse
  never double-counts `active_replicas`. The on-demand claim path keeps the prior
  idempotent-no-op behavior (no patch on every reused claim).
- **Bounded async window warming** (`async_fleet.py`): `sliding`/`pipelined` warm a
  window via `fleet.warm_images` (capped by `max_concurrent`) instead of one
  `to_thread` per image, so a large window can't fan out hundreds of concurrent API
  watches.
- **Registry rewrite normalizes explicit Docker Hub official images**
  (`registry_rewrite.py`): `docker.io/ubuntu` now mirrors to the same
  `library/ubuntu` path as the implicit `ubuntu` form.
- **`warm_images` de-duplicates** its input (`fleet.py`) so a caller's duplicate
  image can't warm the same pool from two threads at once.
- **Capacity pipelined disk estimate is per-node** (`capacity.py`): reports
  `ceil(window/nodes) * replicas * gb * 2` (double-buffer) instead of a cluster
  total, matching the planner's per-node fit budget.
- **Tooling**: the capacity wizard forwards `--cpu-request`/`--max-warmpool-size`
  to the runner (`plan_capacity.py`); the load-test harness closes its fleet even
  if planning fails (`loadtest.py`); the SWE-bench runner rejects
  `--tasks-per-image > 1` (it executes one task/image — use `loadtest.py`).

### Changed / hardening (from PR #1000 review)
- **Prometheus is now an optional `metrics` extra** (`pyproject.toml`,
  `observability.py`): moved off the core deps, and the `asrl_*` collectors are
  registered **lazily** on the first metrics-enabled run rather than at import —
  so importing the package no longer mutates the global Prometheus registry.
  `RunReport` stays always-on and dependency-free.
- **Router-free `exec` no longer leaks clients** (`handles.py`, `cluster.py`):
  reuses a **thread-local** `CoreV1Api` (`Cluster.exec_core_api`) instead of
  building a fresh `ApiClient` (urllib3 pool + thread pool) per call.
- **Scoped WarmPool watch** (`resources.py`): `wait_for_pool_ready` passes
  `field_selector=metadata.name=<name>` so it no longer fans out every other
  pool's events in the namespace.
- **CRD manifest dry-run validation** (`resources.py`, `preflight.py`):
  `Resources.validate_manifests` + a `dry_run` path on `ensure_template` /
  `create_warmpool` server-side dry-run (`dryRun=All`) the hand-built
  SandboxTemplate/WarmPool against the live CRD schema; preflight runs it
  (hard-fails on 400/422 schema rejection, warns otherwise) to catch drift the
  mocked tests can't.
- **`_split_budget` cleanup** (`fleet.py`): compute the weight sum once, drop the
  dead all-zero branch, single-cluster fast path.
- **Docs**: reconciled the removed `r2egym` extra; surfaced the sizing old-vs-new
  table and featured pre-pull in the README.

### Added
- **Config** (`config.py`): `FleetConfig`, `ClusterConfig`, `TemplateSpec`,
  `ResourceSpec`; deterministic `template_name()`.
- **Sizing** (`sizing.py`): concurrency-aware `compute_replicas`,
  `recommend_window`, `plan` (+ `python -m agent_sandbox_rl.sizing` demo).
- **Constants/exceptions**: v1beta1 groups/versions/plurals incl. the
  SandboxTemplate/WarmPool ones the SDK lacks; `FleetError` hierarchy.
- **Multi-cluster** (`cluster.py`): `Cluster` (per-context `ApiClient`, lazy
  SDK `K8sHelper`/`SandboxClient` via attribute injection) + `ClusterRegistry`.
- **Resource CRUD** (`resources.py`): SandboxTemplate/SandboxWarmPool create/
  delete/list + `wait_for_pool_ready` + claim sweep helpers. `wait_for_pool_ready`
  uses a Kubernetes **watch** (event-driven, near-exact readiness timing, no fixed
  poll grid; reconnects + falls back to a short re-check on drops).
- **Environment in RunReport**: `SandboxFleet.describe_environment()` collects
  per-cluster context/namespace/k8s-version/nodes/node-pools/instance-types/region;
  `run()` attaches it to `report.environment` (rendered in `summary()`/`to_dict()`).
  `examples/run_swebench_fleet.py` writes a timestamped `.txt`+`.json` report to
  `REPORT_DIR` when set.
- **Sources** (`sources.py`): `Task`, `TaskSource`, `ListSource`, `JsonlSource`,
  `to_tasks`.
- **Placement** (`placement.py`): `RoundRobin`, `LeastLoaded`,
  `CapacityWeighted`, `ImageAffinity`, capacity-aware, `get_placement`.
- **Handles** (`handles.py`): `SandboxHandle` with `hostname`, `pod_name`,
  `pod_ip`, `endpoint`, router-free `exec` (thread-safe), `release`.
- **Fleet** (`fleet.py`): `SandboxFleet` — `load_tasks`, `preflight`, `plan`,
  `ensure_templates`, `start_warmpools`, `warm_image`/`unwarm_image`, `prepull`,
  `setup`, `acquire`/`acquire_batch`, `handles`/`hostnames`/`endpoints`,
  `release`/`release_all`, `teardown`, `run`, context manager.
- **Strategies + parallelism** (`strategies.py`): `none`/`naive`/`sliding` +
  `process_parallel` (bounded ThreadPool; per-task errors captured).
- **Preflight** (`preflight.py`): per-cluster checks → `PreflightReport`.
- **Pre-pull** (`prepull.py`): DaemonSet image cache (one init container/image).
- **Async** (`async_fleet.py`): `AsyncSandboxFleet` — awaitable parity over the
  sync core (thread-backed; `gather`+`Semaphore`; sync or coroutine `process_fn`).
- **SWE-bench adapter** (`adapters/swebench.py`): `SweBenchSource` (HF dataset),
  `swebench_probe`.
- **Observability** (`observability.py`, design phase 8): always-on `RunReport`
  (per-phase count/total/max, claims, tasks ok/err, warm-replica total+peak;
  `summary()`/`to_dict()`); opt-in Prometheus `asrl_*` series on the default
  registry (`asrl_phase_latency_seconds`, `asrl_task_latency_seconds`,
  `asrl_run_latency_seconds`, `asrl_claims_total`, `asrl_tasks_total`,
  `asrl_warm_replicas`; labels `phase·cluster·family·strategy·status`); opt-in
  OpenTelemetry spans that reuse the SDK's tracer/provider so fleet spans nest
  with the SDK's claim/exec spans; `repo_family()` cardinality bound;
  `serve_metrics()` HTTP helper. Wired through `fleet.py`/`strategies.py`/
  `async_fleet.py`; `ObservabilityConfig` on `FleetConfig.observability`;
  `fleet.report` after `run()`. Prometheus via the optional `metrics` extra
  (lazy collectors, no import side effects); OTel via the `tracing` extra (no-op
  when absent).
- **R2E-Gym adapter** (`adapters/r2egym.py`): `make_fleet_repo_env`,
  `FleetRepoEnv`, `FleetDockerRuntime`, `r2egym_command_files` — bind a
  fleet-pre-warmed pod into R2E-Gym's `RepoEnv` (overriding the cold
  `_start_kubernetes_sandbox`, no-op teardown so the fleet owns the pod, and
  namespace-forwarding exec/copy) so SWE-bench rollouts reuse warm pools and tunix
  deepswe benefits transitively. Lazy/guarded (core imports without R2E-Gym).
  `SweBenchSource(keep_row=True)` stores the full dataset row under
  `metadata["ds"]` (required by the adapter). R2E-Gym isn't on PyPI, so it's
  installed from its checkout — there is no `r2egym` extra.
- **Examples**: `examples/run_swebench_fleet.py` (multi-cluster CLI),
  `examples/deepswe_eval_nb.ipynb` (no-model R2E-Gym-on-warm-pools demo),
  `examples/rl_integration.md` (tunix / R2E-Gym / TorchRL / SkyRL).
- **Docs**: README, `docs/architecture.md`, this changelog.
- **Tests**: 220 mocked unit tests (sizing incl. disk-aware, config, resources incl. watch-based
  pool readiness + fail-fast on terminal errors, cluster, sources, placement,
  fleet incl. 2-cluster routing + acquire rollback + idempotent release,
  strategies/parallel, preflight, prepull, async, swebench incl. `keep_row`,
  r2egym adapter (guard + injected-fake-base override logic + namespace isolation
  under concurrency + bind-failure + thread-safe build), observability incl. the
  RunReport environment block + duplicate-registration guard).

### Hardening (from an internal code review)
- r2egym adapter: namespace forwarded explicitly per-call (no global mutation) so
  concurrent multi-namespace rollouts don't race; thread-safe lazy class build;
  bind-failure surfaced instead of a half-built runtime.
- `fleet.acquire()`: roll back + terminate on partial failure (no leaked sandbox
  / capacity counter); `release()` idempotent under concurrent double-release.
- `wait_for_pool_ready` fails fast on terminal API errors (401/403/404);
  `prepull` returns (with a warning) when no nodes match instead of hanging.
- Observability: Prometheus registration idempotent across re-import; warm-replica
  gauge updated under the lock. Placement: round-robin thread-safe over a stable
  ordering; image-affinity hash order-independent. Async `run()` reports the
  environment block. Misc: SWE-bench `keep_row` deep-copies the row; clearer
  errors for missing image fields and a non-404 namespace preflight result.

### Notes / known follow-ups
- Async backend is a thread-backed wrapper; a native `kubernetes_asyncio` path
  may replace the internals later (API stays the same).
- Candidate upstreams into `k8s-agent-sandbox`: SandboxTemplate/WarmPool
  constants + CRUD, and a `K8sHelper(api_client=...)` parameter.
- Version is dynamic/dev (`0.1.0.dev0`); switch to setuptools-scm on release.
