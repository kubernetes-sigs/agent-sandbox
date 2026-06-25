# Changelog

All notable changes to `agent-sandbox-rl`. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versions are dev-stage.

## [0.1.0.dev0] — unreleased

Initial implementation (design phases 1–7), live-verified on GKE against Agent
Sandbox `v0.5.0rc1` (v1beta1).

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
- **Tests**: 114 mocked unit tests (sizing, config, resources incl. watch-based
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
