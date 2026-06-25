# agent-sandbox-rl

Generic, **multi-cluster** batch orchestration for running SWE-bench-style RL and
evaluation workloads on [Agent Sandbox](https://agent-sandbox.sigs.k8s.io/).

It builds on [`k8s-agent-sandbox`](../../clients/python/agentic-sandbox-client)
and turns the full run lifecycle into a small, framework-agnostic API:

> **load images → configure cluster(s) → compute replicas → preflight → warm
> pools → claim a sandbox per task (hostname/endpoint per sandbox) → run → tear
> down.**

It plugs into any RL stack (R2E-Gym, tunix, TorchRL, SkyRL): the integration
point is a `SandboxHandle` (stable hostname, endpoint, router-free `exec`). Sync
**and** async; low-level **primitives** *and* a managed **runner**; one cluster
or many. Targets the **v1beta1 ("beta")** Agent Sandbox API.

- Design: [`docs/design.md`](docs/design.md)
- Architecture & lifecycle: [`docs/architecture.md`](docs/architecture.md)
- RL-framework integration: [`examples/rl_integration.md`](examples/rl_integration.md)

## Why

`k8s-agent-sandbox` is single-sandbox / single-cluster and has no
SandboxTemplate/WarmPool CRUD, sizing, preflight, pre-pull, or batching — every
consumer re-implements those. `agent-sandbox-rl` provides them once, generically,
across clusters.

## Setup

### 1. Prerequisites

| Requirement | Notes |
| :--- | :--- |
| **Python ≥ 3.10** | The package targets 3.10+. |
| **A Kubernetes cluster** | With the **Agent Sandbox** controller + **v1beta1 extensions** installed (next step). GKE, kind, or any conformant cluster. |
| **`kubectl` + a kube context** | `agent-sandbox-rl` reads your kubeconfig; each `ClusterConfig` selects a context by name (`context=`), or uses the ambient one. |
| **`gke-gcloud-auth-plugin`** | GKE only — must be on `PATH` (`gcloud components install gke-gcloud-auth-plugin`). |
| **Worker node capacity** | Pods land on the nodes your `TemplateSpec` selects (`node_selector`) and, if set, need a matching `runtime_class` (e.g. `gvisor`). |

### 2. Install the Agent Sandbox controller (cluster side)

`agent-sandbox-rl` orchestrates CRDs; it does not install them. The cluster must
already serve the **v1beta1** `SandboxTemplate` / `SandboxWarmPool` /
`SandboxClaim` / `Sandbox` resources. Apply the controller + extensions from a
[release](https://github.com/kubernetes-sigs/agent-sandbox/releases), then verify:

```bash
kubectl get crd | grep agents.x-k8s.io        # expect the 4 CRDs
kubectl get pods -n agent-sandbox-system       # controller Running
```

`fleet.preflight()` checks all of this for you and raises `PreflightError` with a
precise message if something is missing.

### 3. Install the Python packages (client side)

Both the SDK and this package are installed editable from the repo. Run from the
**repo root**:

```bash
# core: SDK + this package
pip install -e clients/python/agentic-sandbox-client \
            -e examples/agent-sandbox-rl

# with the SWE-bench dataset loader (recommended for SWE-bench runs)
pip install -e clients/python/agentic-sandbox-client \
            -e 'examples/agent-sandbox-rl[swebench]'
```

### Dependencies & extras

Core deps (installed automatically): `k8s-agent-sandbox` (the SDK — **reused, not
forked**), `kubernetes`, and `pydantic>=2`. That's it — the always-on `RunReport`
is dependency-free, and merely importing the package registers no Prometheus
collectors (Prometheus is the optional `metrics` extra below).

| Extra | Pulls in | Use it for |
| :--- | :--- | :--- |
| `swebench` | `datasets` (Hugging Face) | `SweBenchSource` — loading SWE-bench task lists. |
| `async` | `k8s-agent-sandbox[async]`, `kubernetes_asyncio` | `AsyncSandboxFleet` on an asyncio loop. |
| `metrics` | `prometheus-client` | Export `asrl_*` Prometheus series (`enable_metrics=True`). Without it, `RunReport` still works. |
| `tracing` | `opentelemetry-api` / `-sdk` / `-exporter-otlp` (~=1.39) | OpenTelemetry span export (`enable_tracing=True`). No-op when absent. |
| `test` | `pytest`, `pytest-asyncio`, `pytest-xdist` | Running the mocked unit tests. |

Combine extras with commas, e.g. `…/agent-sandbox-rl[swebench,async,metrics]`.

The **R2E-Gym adapter** (`adapters.r2egym`) needs R2E-Gym, which isn't on PyPI —
install it from its checkout (`pip install -e path/to/R2E-Gym`); the adapter
raises a clear error if it's missing. (No `r2egym` extra for that reason.)

### 4. Verify

```bash
# unit tests (mocked, no cluster needed)
pytest examples/agent-sandbox-rl

# import + reach your cluster
python -c "import agent_sandbox_rl as a; print('agent-sandbox-rl', a.__version__)"
python -c "from agent_sandbox_rl import SandboxFleet, FleetConfig, ClusterConfig; \
SandboxFleet(FleetConfig(clusters=[ClusterConfig(name='c', namespace='default')])).preflight()"
```

A clean `preflight()` (no `PreflightError`) means you're ready for the Quickstart.

## Quickstart

### Managed runner (simplest)

```python
from agent_sandbox_rl import SandboxFleet, FleetConfig, ClusterConfig, SweBenchSource, swebench_probe

fleet = SandboxFleet(FleetConfig(
    clusters=[ClusterConfig(name="rl", namespace="rl-tunix-swebench")],
    max_concurrent=8, max_warmpool_size=32, placement="image-affinity"))
fleet.load_tasks(SweBenchSource(limit=8))

# strategy: none | naive | sliding   (concurrency defaults to max_concurrent)
results = fleet.run(swebench_probe, strategy="sliding", concurrency=8)
```

### Primitives (RL loop owns the schedule)

```python
fleet.load_tasks([{"id": "t1", "image": "busybox:1.36"}])
fleet.setup()                         # preflight → plan → warm pools
for task in fleet.tasks:
    h = fleet.acquire(task)           # claim a pre-warmed sandbox
    try:
        print(h.hostname, h.endpoint())
        print(h.exec(["sh", "-c", "echo hi $(hostname)"]))   # router-free
    finally:
        fleet.release(h)
fleet.teardown()
# or: `with fleet: ...`  (setup on enter, teardown on exit)
```

### Async

```python
from agent_sandbox_rl import AsyncSandboxFleet
fleet = AsyncSandboxFleet(cfg); fleet.load_tasks(src)
results = await fleet.run(async_or_sync_process_fn, strategy="naive", concurrency=64)
# or: async with fleet: h = await fleet.acquire(task); ...
```

### CLI example

```bash
cd examples
WARMPOOL_STRATEGY=sliding TASKS_LIMIT=4 MAX_CONCURRENT=4 NAMESPACE=rl-tunix-swebench \
NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=e2-pool \
python run_swebench_fleet.py
```

## Concepts

| Concept | What it is |
| :--- | :--- |
| **Task** | `id` + container `image` + opaque `metadata`. Generic unit of work. |
| **TaskSource** | Produces tasks: `ListSource`, `JsonlSource`, `SweBenchSource` (HF). |
| **FleetConfig** | Clusters + orchestration knobs (concurrency, sizing, placement, template). |
| **ClusterConfig** | One target cluster (context/kubeconfig, namespace, node selector, runtime class, pull secret, weight, capacity). |
| **SandboxFleet** / **AsyncSandboxFleet** | The orchestrator (sync / async). |
| **SandboxHandle** | A claimed sandbox: `hostname`, `pod_name`, `pod_ip`, `endpoint(port)`, `exec(cmd)`, `release()`. |
| **Placement** | Which cluster serves an image: `round-robin`, `least-loaded`, `capacity-weighted`, `image-affinity`. |
| **Strategy** | *When* pools exist: `none`, `naive`, `sliding`. |
| **Adapters** | Framework glue: `adapters.swebench` (dataset → tasks), `adapters.r2egym` (`make_fleet_repo_env` binds a warm pod into R2E-Gym/tunix `RepoEnv`). |

## Warm-pool strategies

| Strategy | Behavior | Footprint |
| :--- | :--- | :--- |
| `naive` | Pre-warm every image up front; process all (parallel); tear down. | Highest (all pools at once). |
| `sliding` | Keep only a window of image pools warm, rolling forward. | Bounded (~`window`); window auto-sizes to `max_concurrent`. |
| `none` | One size-1 pool per image on demand, torn down after. | Lowest (cold-start per image). |

## Replica sizing

Pool depth is the image's share of the concurrency budget, not its task count:

```text
replicas_image = clamp(round(MAX_CONCURRENT × tasks_image / tasks_total),
                       1, min(tasks_image, MAX_WARMPOOL_SIZE))
```

`MAX_CONCURRENT` is the one knob that both **sizes pools** and **parallelizes
claim+exec**. This is the core cost win — it avoids warming *N* pods for *N* tasks
while keeping sub-second claims. `python -m agent_sandbox_rl.sizing` prints the
old-vs-new footprints; for a skewed 100-task / 8-image batch (`MAX_WARMPOOL_SIZE=32`):

| `MAX_CONCURRENT` | baseline `min(count, cap)`, all warm | concurrency-aware footprint | sliding window |
| ---: | ---: | ---: | ---: |
| 1 | 92 pods | **8 pods** | 1 |
| 8 | 92 pods | **11 pods** | 5 |
| 32 | 92 pods | **32 pods** | 8 |
| 256 | 92 pods | 92 pods | 8 |

The naive (warm-everything) baseline holds 92 pods regardless; sizing pools to the
concurrency budget cuts that to 8–32 for the same throughput, and `sliding` bounds
it further to a window.

## Multi-cluster

Pass several `ClusterConfig`s (different `context`/`kubeconfig`) + a `placement`;
the fleet builds a per-context client for each, distributes pools/claims, and each
`SandboxHandle` carries its owning cluster. Cross-cluster reachability is the
caller's concern (see the integration guide).

## Configuration reference

**FleetConfig:** `clusters`, `placement`, `max_concurrent` (1), `max_warmpool_size`
(8), `window_size` (None=auto), `ready_timeout` (900), `template` (`TemplateSpec`),
`template_name_prefix` (`r2e-img-`), `labels`.

**ClusterConfig:** `name`, `kubeconfig`, `context`, `in_cluster`, `namespace`,
`node_selector`, `runtime_class`, `image_pull_secret`, `weight`, `max_replicas`.

**TemplateSpec:** `resources` (cpu/memory), `keepalive_command` (`sleep infinity`),
`runtime_class`, `node_selector`, `image_pull_secret`, `extra_pod_spec`.

## Operational features

- **Preflight** (`fleet.preflight()`): per-cluster reachability, v1beta1 CRD
  versions, controller, namespace, and (if configured) runtime class + pull
  secret. Hard failures raise `PreflightError`; soft issues are warnings.
- **Pre-pull** (`fleet.prepull()` / `setup(prepull=True)`): a DaemonSet caches
  task images on every node so warm pools skip the multi-GB pull. This is where
  cold-start time goes — `wait_pool_ready` dominates a cold run (the sample report
  shows it as ~34 s of a 48 s run), so pre-pulling (or a persistent node-level
  image cache) is the single biggest lever for repeated/RL runs.
- **Watch-based readiness**: `wait_for_pool_ready` watches the WarmPool and
  returns at the `readyReplicas` event (near-exact timing, no fixed poll grid),
  reconnecting and falling back to a short re-check on watch drops.
- **Cleanup**: everything created is labeled `app=agent-sandbox-rl`; `teardown`
  sweeps claims → pools → templates (defensive against stray claims).

## Observability

Three layers, mirroring the `k8s-agent-sandbox` SDK so traces/metrics interoperate:

1. **`RunReport`** — always-on, dependency-free. `fleet.run(...)` records per-phase
   timings (preflight, plan, create_warmpool, wait_pool_ready, claim, process,
   release, teardown), claims, tasks ok/err, warm-replica total+peak, and an
   `environment` block (cluster context/namespace/k8s-version/nodes/node-pools/
   instance-types/region, via `fleet.describe_environment()`).

   ```python
   results = fleet.run(probe, strategy="naive")
   print(fleet.report.summary())     # benchmark table (also logged at INFO)
   data = fleet.report.to_dict()     # JSON-friendly
   ```
   ```text
   ── Run report (strategy=naive) ──
     environment:
       default: context=(ambient)  namespace=rl-tunix-swebench  k8s_version=v1.35...
                nodes=11  node_pools=[e2-pool,...]  region=us-central2
     preflight              1.35s  (n=1, max=1.35s)
     wait_pool_ready        8.44s  (n=2, max=4.22s)
     claim                  5.66s  (n=4, max=1.69s)
     process                1.56s  (n=4, max=0.40s)
     ...
     TOTAL                 14.71s
     claims=4  tasks=4ok/0err  warm_replicas total=3 peak=3
   ```

   `examples/run_swebench_fleet.py` writes a timestamped `.txt` + `.json` report
   to `REPORT_DIR` when that env var is set. See
   [`performance_reports/README.md`](performance_reports/README.md) for a full
   breakdown of every phase and metric. (Note: per-phase totals are *summed*
   durations, so under concurrency they exceed the wall-clock `TOTAL`.)

2. **Prometheus metrics** (opt-in, default **on**; needs the `metrics` extra) —
   `asrl_*` series on the default registry. The collectors are registered lazily
   on the first metrics-enabled run, so importing the package has no global side
   effect (and with `prometheus-client` not installed, metrics are a silent
   no-op while `RunReport` keeps working). Series:
   `asrl_phase_latency_seconds`, `asrl_task_latency_seconds`,
   `asrl_run_latency_seconds` (histograms), `asrl_claims_total`,
   `asrl_tasks_total` (counters), `asrl_warm_replicas` (gauge). Labels are
   bounded: `phase · cluster · family · strategy · status` (`family` is the
   repo family, not the per-image tag). Expose them with the built-in helper:

   ```python
   from agent_sandbox_rl import serve_metrics
   server, _ = serve_metrics(port=9095)   # GET /metrics ; caller owns lifetime
   ```

3. **OpenTelemetry spans** (opt-in, default off; needs the `tracing` extra) —
   reuse the SDK's tracer/provider so fleet `asrl.*` spans nest with the SDK's
   `create_claim`/`wait_for_sandbox_ready` spans in one trace.

   ```python
   FleetConfig(..., observability=ObservabilityConfig(
       enable_metrics=True, enable_tracing=True))
   ```

   (`asyncio.to_thread` doesn't auto-propagate OTel context — under
   `AsyncSandboxFleet`, metrics + `RunReport` are exact; nested SDK spans are a
   documented follow-up.)

## Troubleshooting

| Symptom | Cause / fix |
| :--- | :--- |
| `PreflightError: ... crd:* not found` | Agent Sandbox extensions not installed — apply the controller + extensions. |
| Claims never resolve / pods `Pending` | Node selector unsatisfiable, or `runtimeClassName` (e.g. gvisor) with no matching nodes. |
| Docker Hub `429` on image pulls | Set `image_pull_secret`, or mirror images to a registry / use pre-pull. |
| `'NoneType' object has no attribute 'decode'` on parallel exec | Handled: `SandboxHandle.exec` builds a fresh `ApiClient` per call (kubernetes `stream()` isn't thread-safe across a shared client). |
| Async `process_fn` calls `handle.exec` | `exec` is blocking — in async code do `await asyncio.to_thread(h.exec, ...)`, or pass a sync `process_fn` (run in a worker thread automatically). |

## Testing

```bash
pytest examples/agent-sandbox-rl   # mocked, no cluster
```

This suite is also wired into the repo's unit-test runner
(`dev/tools/test-unit`, run by `make test-unit` and the `unit-test` presubmit),
so regressions are caught in CI — it spins up a venv, installs the in-repo SDK +
this package's `[test]` extra, and runs the mocked tests.

## Status

Phases 1–8 implemented and live-verified on GKE (agent-sandbox `v0.5.0rc1`):
config/sizing, multi-cluster, template/warm-pool CRUD, sources/placement/handles,
fleet primitives, strategies + parallel execution, preflight, pre-pull, async,
the SWE-bench adapter + example, and observability (RunReport + Prometheus +
OpenTelemetry). See [`docs/architecture.md`](docs/architecture.md)
and [`CHANGELOG.md`](CHANGELOG.md).
