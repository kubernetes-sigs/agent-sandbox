# Performance reports

Per-run performance reports emitted by the `agent-sandbox-rl` package (the
`RunReport` produced by `SandboxFleet.run(...)`). Each run writes two files via
`examples/run_swebench_fleet.py` when `REPORT_DIR` is set:

```
<strategy>_<n>tasks_<YYYYMMDD-HHMMSS>.txt    # human-readable summary table
<strategy>_<n>tasks_<YYYYMMDD-HHMMSS>.json   # same data, machine-readable (report.to_dict())
```

> The one checked-in sample here has its `environment` block **redacted**
> (`<namespace>`, `<region>`, …) — it only illustrates the format. Don't commit
> real run reports (they embed live cluster metadata and churn); this directory is
> for the format docs + a redacted example.

Generate one:

```bash
cd examples/agent-sandbox-rl
PYTHONPATH="$(pwd)" \
WARMPOOL_STRATEGY=sliding TASKS_LIMIT=10 MAX_CONCURRENT=5 \
NAMESPACE=rl-tunix-swebench \
NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=e2-pool \
REPORT_DIR="$(pwd)/performance_reports" \
python examples/run_swebench_fleet.py
```

## Anatomy of a report

```
── Run report (strategy=sliding) ──
  environment:
    default: context=(ambient)  namespace=<namespace>  k8s_version=<k8s-version>
             nodes=<n>  node_pools=[<node-pool>]
             instance_types=[<instance-type>]  region=<region>
  ----------------------------------------
  preflight              0.84s  (n=1, max=0.84s)
  plan                   0.00s  (n=1, max=0.00s)
  create_warmpool        4.79s  (n=10, max=0.59s)
  wait_pool_ready       34.09s  (n=10, max=7.29s)
  claim                 13.77s  (n=10, max=1.82s)
  process                5.88s  (n=10, max=0.61s)
  release                1.27s  (n=10, max=0.14s)
  teardown               0.37s  (n=1, max=0.37s)
  ----------------------------------------
  TOTAL                 47.87s
  claims=10  tasks=10ok/0err  warm_replicas total=10 peak=5
```

### Reading a phase line

```
  wait_pool_ready       34.09s  (n=10, max=7.29s)
   └ phase             └ total   └ count └ slowest single occurrence
```

- **total** — sum of the wall time spent in that phase across **all** its
  occurrences in the run (seconds).
- **n** — how many times the phase ran (e.g. one `claim` per task, one
  `create_warmpool` per image pool, one `preflight` per run).
- **max** — the slowest single occurrence — useful for spotting tail latency
  (one slow image pull, one slow claim) that an average would hide.

> **Important — totals are summed, not wall-clock.** Under concurrency
> (`MAX_CONCURRENT`) and sliding windows, `claim`/`process` run in parallel and
> pools warm in overlapping waves, so the **sum of phase totals usually exceeds
> `TOTAL`** (the actual end-to-end wall time). Use per-phase totals to compare
> where time goes; use `TOTAL` for real elapsed time.

## Phases (lifecycle order)

| Phase | What it measures |
| :--- | :--- |
| `preflight` | Per-cluster checks: reachability, v1beta1 CRDs present, controller, namespace, and (if configured) runtime class / pull secret. |
| `plan` | Compute the run plan: map each unique image → a cluster (placement) and size its warm-pool replicas. Pure bookkeeping, usually ~0s. |
| `prepull` | *(only if enabled)* Wait for the DaemonSet that pre-pulls task images onto every node so warm pods skip the multi-GB pull. |
| `create_warmpool` | Create the `SandboxTemplate` + `SandboxWarmPool` objects for an image pool (the API calls only, not the pull). One per pool. |
| `wait_pool_ready` | Block until the pool reports `readyReplicas` (the pods are pulled, started, Ready). **Detected via a Kubernetes watch** — near-exact, no poll-grid rounding. This is the cold-start cost and is dominated by the image pull on a fresh node. |
| `claim` | Acquire a sandbox for a task: create a `SandboxClaim`, resolve it to a `Sandbox`, wait Ready, build the handle. One per task. Sub-second when pools are warm. |
| `process` | Your `process_fn` running inside the sandbox (here the SWE-bench probe: a router-free `exec`). One per task. |
| `release` | Release a sandbox (delete its `SandboxClaim`; the controller reaps/replaces the pod). One per task. |
| `teardown` | Sweep everything the run created — claims → pools → templates — by the `app=agent-sandbox-rl` label. |

## Footer counters

```
  claims=10  tasks=10ok/0err  warm_replicas total=10 peak=5
```

| Field | Meaning |
| :--- | :--- |
| `claims` | Total `SandboxClaim`s created during the run (one per task here). |
| `tasks NNok/MMerr` | Tasks whose `process_fn` returned (`ok`) vs raised (`err`). A per-task failure is captured, not fatal. |
| `warm_replicas total` | Cumulative warm replicas created across the whole run (sums every pool ever warmed). |
| `warm_replicas peak` | Max warm replicas alive **at once** — the real idle footprint. This is where strategies differ: `naive` keeps every pool warm (peak ≈ total), `sliding` bounds it to the window (peak < total), `none` ≈ the window of size-1 pools. |

## Environment block

Best-effort cluster details collected by `SandboxFleet.describe_environment()`
at run start (omitted if the cluster can't be queried):

| Field | Meaning |
| :--- | :--- |
| `context` | Kubeconfig context the cluster used (`(ambient)` = current context). |
| `namespace` | Namespace the run's objects were created in. |
| `k8s_version` | API server git version. |
| `nodes` | Total nodes in the cluster (all pools, not just where pods landed). |
| `node_pools` | Distinct node-pool labels present (`cloud.google.com/gke-nodepool`). |
| `instance_types` | Distinct machine types (`node.kubernetes.io/instance-type`). |
| `region` | Cluster region (`topology.kubernetes.io/region`). |

## JSON schema (`.json`)

Mirrors the `.txt`, machine-readable (`RunReport.to_dict()`):

```json
{
  "strategy": "sliding",
  "total_s": 47.87,
  "phases": {
    "wait_pool_ready": {"count": 10, "total_s": 34.09, "max_s": 7.29}
  },
  "claims": 10,
  "tasks_ok": 10,
  "tasks_err": 0,
  "warm_replicas_total": 10,
  "warm_replicas_peak": 5,
  "environment": { "default": { "nodes": "<n>", "node_pools": ["<node-pool>"] } }
}
```

## See also

- Package architecture & lifecycle: [`../docs/architecture.md`](../docs/architecture.md)
- Observability internals (RunReport, Prometheus, tracing): the *Observability*
  section of [`../README.md`](../README.md).
- The script-based prototype's hand-measured notes live in the companion
  `rl-sandbox-scripts` example (`performance.md`), a separate contribution.
