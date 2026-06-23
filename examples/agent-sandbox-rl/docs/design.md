# Design: `agent-sandbox-rl` — generic, multi-cluster batch orchestration for RL on Agent Sandbox

Status: implemented. The original design doc, kept (versioned with the example)
as the rationale behind the package as built.

## Context

The `k8s-agent-sandbox` Python client is **single-sandbox, single-cluster**:
`SandboxClient.create_sandbox(warmpool=...)` claims one pre-existing warm pool on
the ambient kube context; it has **no** SandboxTemplate/SandboxWarmPool CRUD, no
batch orchestration, no sizing, no preflight, no pre-pull, and **no notion of
multiple clusters**. Every consumer (this `rl-sandbox-scripts` example, tunix
`eval_deepswe.py`, the R2E-Gym `kubernetes-sandbox` backend) re-implements this.

Goal: a **new, separate pip package `agent-sandbox-rl`** (import
`agent_sandbox_rl`) that depends on `k8s-agent-sandbox` and provides the full RL
run lifecycle — load images → configure **one or many clusters** → templates /
pools → compute replicas → preflight → setup → start warm pools → start claims →
**return a hostname/endpoint per sandbox** → teardown — as a **generic,
framework-agnostic** API that plugs into any RL stack (R2E-Gym, tunix, TorchRL,
SkyRL). It can **control and connect to multiple clusters** (scale past one
cluster's CPU/disk limits, multi-region, tenant isolation). SWE-bench specifics
live in a thin adapter. Sync **and** async; both low-level primitives and a
managed batch runner.

Generalizes the cluster-verified code in this example (`warmpool.py`,
`sizing.py`, `strategies.py`, `prepull.sh`) and keeps its measured wins
(sub-second claims, concurrency-aware sizing, pre-pull).

## Packaging

All code lives under this example:
`examples/agent-sandbox-rl/`.
- `pyproject.toml`: `name = "agent-sandbox-rl"`, import package `agent_sandbox_rl`,
  explicit dev version (`0.1.0.dev0`; switch to setuptools-scm on release). Deps:
  `k8s-agent-sandbox`, `kubernetes`, `pydantic>=2`. Extras: `swebench` →
  `datasets`; `metrics` → `prometheus-client` (lazy, no import side effects);
  `async` → `k8s-agent-sandbox[async]`, `kubernetes_asyncio<34.0.0`; `tracing` →
  `opentelemetry-api/sdk/exporter-otlp`; `test` → `pytest`, `pytest-asyncio`,
  `pytest-xdist`.

## Package layout

```
examples/agent-sandbox-rl/
  pyproject.toml, README.md
  agent_sandbox_rl/
    __init__.py          # public exports
    config.py            # FleetConfig, ClusterConfig, TemplateSpec, ResourceSpec (pydantic)
    cluster.py           # Cluster (per-context clients) + ClusterRegistry; multi-cluster kube config loading
    placement.py         # Placement protocol + RoundRobin, LeastLoaded, CapacityWeighted, ImageAffinity
    constants.py         # TEMPLATE/WARMPOOL group/version/plurals (missing from the SDK)
    resources.py         # SYNC SandboxTemplate + SandboxWarmPool CRUD + wait_for_pool_ready (watch-based, per-cluster)
    async_resources.py   # async variant
    sizing.py            # compute_replicas, recommend_window, plan (ported)
    sources.py           # Task model + TaskSource protocol; ListSource, JsonlSource
    handles.py           # SandboxHandle (carries its owning Cluster + connection info)
    preflight.py         # per-cluster checks (reachable, CRDs+versions, runtimeclass, capacity, pull-secret)
    prepull.py           # optional DaemonSet image pre-pull per cluster (AppsV1Api), sync+async
    strategies.py        # Strategy protocol + none/naive/sliding (cluster-aware, drive fleet primitives)
    fleet.py             # SandboxFleet (sync orchestrator: primitives + run())
    async_fleet.py       # AsyncSandboxFleet (parity; real concurrent claim/exec across clusters)
    exceptions.py        # FleetError, PreflightError, CapacityError, NoClusterAvailableError
    adapters/
      swebench.py        # SweBenchSource (HF), swebench template profile, /testbed probe
  examples/
    run_swebench_fleet.py            # parity with current run_swebench.py, multi-cluster aware
    rl_integration.md                # tunix / R2E-Gym / TorchRL / SkyRL plug-in patterns
  tests/                             # pytest (+ pytest-asyncio), mocked k8s
```

## Multi-cluster model (the core addition)

- **`ClusterConfig`** (pydantic, per cluster): `name`, `kubeconfig` (path|None),
  `context` (str|None), `in_cluster` (bool), `namespace`, `node_selector`,
  `runtime_class`, `image_pull_secret`, `connection` (SDK
  `SandboxConnectionConfig`), `weight` (float, for weighted placement),
  `capacity` (optional max replicas / cpu hints).
- **`Cluster`** (runtime): built from a `ClusterConfig`. Owns its **own**
  `kubernetes.client.ApiClient` via
  `config.new_client_from_config(config_file=kubeconfig, context=context)` (or
  in-cluster), and from it: `CustomObjectsApi`, `CoreV1Api`, `AppsV1Api`, a
  per-cluster `resources` manager, and a per-cluster SDK `K8sHelper` +
  `SandboxClient` (pointed at this context by **attribute injection** —
  `helper.custom_objects_api/core_v1_api = <per-cluster apis>`, `client.k8s_helper
  = helper` — so **no SDK fork is required**; a clean `api_client`/`context`
  param is a nice-to-have to upstream later). Tracks live pools/claims/replicas
  for placement + capacity.
- **`ClusterRegistry`**: holds N `Cluster`s; lookup by name; aggregates status.
- **`Placement`** protocol: `select(task, registry, state) -> Cluster`. Builtins:
  `RoundRobin`, `LeastLoaded` (fewest active replicas/claims), `CapacityWeighted`
  (by `weight`/capacity), `ImageAffinity` (route an image/repo-family to a
  consistent cluster to reuse cached layers + warm pools — ties to the
  layer-sharing finding). Default: `ImageAffinity` then `LeastLoaded` fallback.
- **Sizing across clusters:** the global `max_concurrent` budget is split across
  clusters by weight/capacity; per-`(cluster, image)` replicas use the existing
  `sizing.compute_replicas` on that cluster's share.
- **Connectivity caveat (documented, not magic):** a sandbox is reachable from
  *within its own cluster*. A single learner spanning clusters needs per-cluster
  routable endpoints (Gateway/LoadBalancer per cluster, or VPC peering/VPN), or
  co-located workers per cluster. Each `SandboxHandle` carries its cluster's
  connection config so the framework connects correctly; in-cluster learners use
  the sandbox's stable DNS, cross-cluster learners use the per-cluster
  Gateway/Direct endpoint.

Single-cluster is just `N=1` (a default local cluster from the ambient context),
so the simple case stays simple.

## Core abstractions

- **`Task`** (`sources.py`): `id`, `image`, `metadata` (generic; SWE-bench maps
  rows → `Task(id=instance_id, image=docker_image, metadata={repo,...})`).
- **`TaskSource`** protocol `load() -> list[Task]`; builtins `ListSource`,
  `JsonlSource`; `SweBenchSource` (HF) in `adapters/swebench.py` (`swebench` extra).
- **`FleetConfig`** (pydantic): `clusters: list[ClusterConfig]`, `placement`,
  `max_concurrent`, `max_warmpool_size`, `window_size` (None=auto),
  `ready_timeout`, `template` (`TemplateSpec`), `template_name_fn`
  (default `r2e-img-<md5[:12]>`), `labels`.
- **`SandboxHandle`** (`handles.py`): per claim — `task`, **`cluster`** (name +
  connection), `claim_name`, `sandbox_id`, **`hostname`** (stable in-cluster DNS),
  `pod_name` (`agents.x-k8s.io/pod-name`), `pod_ip`, `endpoint`, the SDK
  `Sandbox` (`.commands`/`.files`), plus `.exec(cmd)` and `.release()`.

## Public API (maps to requested ops; sync `SandboxFleet` / async `AsyncSandboxFleet`)

| Requested op | Method |
| :--- | :--- |
| k8s_cluster_config_set | `add_cluster(ClusterConfig)` / `set_clusters([...])` / classmethods `from_kubeconfig(contexts=[...])`, `from_incluster()` — builds a `Cluster` per context |
| load_image_list | `load_tasks(source | list[Task] | list[str])` |
| preflight checks | `preflight() -> dict[cluster, PreflightReport]` (raises on hard failures) |
| compute replicas | `plan() -> FleetPlan` (per-cluster, per-image replicas + window) |
| set templates | `ensure_templates()` (across selected clusters) |
| pools / start warmpools | `start_warmpools(wait=True)` (distributes pools across clusters per placement+sizing) |
| (optional) pre-pull | `prepull(wait=True)` / `prepull_delete()` (per cluster) |
| setup | `setup()` — preflight → plan → (optional prepull) → start warm pools |
| start claims | `acquire(task) -> SandboxHandle`, `acquire_batch(tasks) -> [SandboxHandle]` (placement-routed) |
| return hostname list | `handles()`, `hostnames()`, `endpoints()` (cluster-qualified) |
| teardown / delete | `release(handle)`, `release_all()`, `teardown(delete_namespace=False)` (routes to owning cluster) |
| managed batch | `run(process_fn, strategy=None) -> [Result]` |
| ergonomics | context manager `__enter__/__exit__` → `setup()`/`teardown()` |

**Two modes (both shipped):** (1) **primitives** an RL loop drives itself
(`acquire` → use `handle.hostname/endpoint` → `release`); (2) **managed runner**
`fleet.run(process_fn, strategy=...)` drives provisioning timing + claim fan-out
across clusters (the async runner does real concurrent claim+exec = optimization
#3).

## Reuse map (don't reinvent)

- **Claiming:** reuse SDK `SandboxClient.create_sandbox` / `Sandbox.get_pod_name`
  / `get_pod_ip` / `terminate` and `K8sHelper` (`resolve_sandbox_name`,
  `wait_for_sandbox_ready`, `list_sandbox_claims`) — one set per cluster via
  attribute injection.
- **Template/WarmPool CRUD = the new code:** port this example's `warmpool.py`
  into `resources.py` (per-cluster `CustomObjectsApi`; constants in
  `constants.py`).
- **Sizing/strategies/pre-pull:** port `sizing.py`, `strategies.py`,
  `prepull.sh` → `prepull.py`, made cluster-aware.

## Pluggability into RL frameworks

Framework-agnostic `SandboxHandle`s + `examples/rl_integration.md`: tunix
(replace the `eval_deepswe` warm-pool block with `fleet.setup()` + `acquire`),
R2E-Gym (`kubernetes-sandbox` backend → `fleet.acquire`), TorchRL/SkyRL (env wraps
`acquire`/`release`; `handle.endpoint` is the connection target). SWE-bench probe
ships in `adapters/swebench.py` as the default runner `process_fn`.

## Testing

Unit (mocked, no cluster): `sizing` (pure), `resources`/`prepull` (mock
`CustomObjectsApi`/`AppsV1Api`+watch), `placement`, `cluster`/registry (mock
`new_client_from_config`), `strategies`, `fleet`/`async_fleet` (mock per-cluster
`SandboxClient`/`K8sHelper`), incl. a **2-cluster** routing test. Mirror SDK test
patterns (`unittest.mock`, `pytest-asyncio`).

## Verification (live)

1. `pip install -e clients/python/agentic-sandbox-client -e 'examples/agent-sandbox-rl[swebench]'`; `pytest` green (sync+async).
2. **Single-cluster smoke** (the GKE cluster we use): `SweBenchSource(limit=2)` →
   `from_kubeconfig` → `preflight` → `setup(naive)` → `acquire_batch` →
   `hostnames()` → `/testbed` probe → `teardown`; namespace empty after.
3. **Multi-cluster smoke:** register the same cluster twice under two
   contexts/namespaces (proxy for 2 clusters) → confirm placement spreads
   pools/claims, `hostnames()` are cluster-qualified, teardown routes correctly.
4. **Async** parity smoke (concurrent acquire+exec).
5. **Parity:** `examples/run_swebench_fleet.py -s sliding -n 10` reproduces
   `performance.md` v2 from the companion `rl-sandbox-scripts` example (claims=10,
   sized pools, ~115 s).

## Implementation order

1. Scaffold + `pyproject.toml` + `constants.py` + `config.py` + `sizing.py` (+tests).
2. `cluster.py` (per-context clients + registry) + `resources.py` (+mocked tests).
3. `placement.py` + `handles.py` + `fleet.py` primitives (+tests, incl. 2-cluster).
4. `strategies.py` + `fleet.run()` runner (+tests).
5. `preflight.py`, `prepull.py`.
6. `async_resources.py` + `async_fleet.py` parity (+async tests).
7. `adapters/swebench.py` + `examples/run_swebench_fleet.py` + `rl_integration.md`.
8. Live verification (single + multi-cluster + async + parity).
9. **Documentation** — thorough, complete package docs: module/class/function
   docstrings (Google style) across the whole API; a rich `README.md`
   (concepts, quickstart, multi-cluster, strategies, sizing, RL-framework
   integration, API reference table, troubleshooting); a `docs/` guide
   (architecture + lifecycle + the optimization findings); runnable docstring
   examples; and `CHANGELOG.md`.
10. **Observability** (`observability.py`) — always-on `RunReport` (per-phase
    count/total/max + claims/tasks/warm peak + an `environment` block from
    `describe_environment()`; `summary()`/`to_dict()`, persisted to `REPORT_DIR`
    by the example runner); opt-in Prometheus `asrl_*` series (labels
    `phase·cluster·family·strategy·status`, `repo_family` cardinality bound) with a
    `serve_metrics()` helper; opt-in OpenTelemetry spans reusing the SDK's tracer
    so fleet spans nest with the SDK's claim/exec spans. Mirrors the SDK's
    observability pattern. (`wait_for_pool_ready` is watch-based for near-exact
    readiness timing.)

Notes: keep core deps minimal (HF `datasets` only via the `swebench` extra); the
template/warmpool constants + CRUD and a `K8sHelper(api_client=...)` param are
candidates to upstream into `k8s-agent-sandbox`.
