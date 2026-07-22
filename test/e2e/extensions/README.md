# Runtime Class Benchmark Tests

Runtime-class-aware e2e tests and benchmarks for `sigs.k8s.io/agent-sandbox`.
They measure cold start latency, warm pool claim speed, and burst recovery
behaviour across different container runtimes (runc, gVisor, kata).

## Prerequisites

- A Kubernetes cluster with agent-sandbox deployed (CRDs + controller).
- `KUBECONFIG` pointing at the cluster.
- For gVisor tests: RuntimeClass `gvisor` installed. On OpenShift, gVisor can
  be installed on worker nodes even though it is not included natively.
- For kata tests: RuntimeClass `kata` (or `kata-remote`) installed via the
  sandboxed-containers operator or equivalent.

## Tests and Benchmarks

| Name | Type | What it measures |
|------|------|------------------|
| `TestRuntimeClassLifecycle` | Test | Full SandboxTemplate → WarmPool → SandboxClaim lifecycle with a given RuntimeClass |
| `TestRuntimeClassStartupComparison` | Test | Cold start vs warm claim side-by-side, reports speedup ratio |
| `TestRuntimeClassBurstRecovery` | Test | Sustained batch load against various pool sizes, writes per-claim CSV reports with quality zone stats |
| `BenchmarkRuntimeClassColdStart` | Benchmark | Raw cold sandbox creation latency per image (`sandbox-ready-sec` metric) |
| `BenchmarkRuntimeClassWarmClaim` | Benchmark | Warm pool claim latency across image × pool-size combinations (`claim-ready-sec` metric) |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_RUNTIME_CLASS` | *(required)* | RuntimeClass name: `default` (cluster default / runc), `gvisor`, `kata`, etc. Tests skip when unset. |
| `SANDBOX_POOL_SIZES` | total worker CPUs | Comma-separated pool sizes for burst recovery and warm claim benchmarks. Defaults to the cluster's total worker CPU count when unset. |
| `SANDBOX_REPORT_DIR` | `.` (cwd) | Base directory for CSV output. A subdirectory is auto-created per run. |
| `SANDBOX_CLUSTER_ID` | *(auto-detected)* | Override cluster identity string in report paths |
| `SANDBOX_WORKLOAD_SEC` | `30` | Seconds the workload container sleeps (simulates real work). `0` uses a pause container. |
| `SANDBOX_IMAGES` | `registry.k8s.io/pause:3.10` | Comma-separated images for cold start and warm claim benchmarks |

## Quick Start

All commands assume you are in the repo root with `KUBECONFIG` set.

### runc (cluster default)

```shell
# Lifecycle smoke test
SANDBOX_RUNTIME_CLASS=default \
  go test ./test/e2e/extensions/... -run TestRuntimeClassLifecycle -v -timeout 5m

# Cold vs warm comparison
SANDBOX_RUNTIME_CLASS=default \
  go test ./test/e2e/extensions/... -run TestRuntimeClassStartupComparison -v -timeout 5m

# Burst recovery with CSV output (pool sizes 4,8,12,16,20,24)
SANDBOX_RUNTIME_CLASS=default \
  SANDBOX_POOL_SIZES=4,8,12,16,20,24 \
  go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 30m

# Cold start benchmark (5 iterations)
SANDBOX_RUNTIME_CLASS=default \
  go test -v -run='^$' -bench=BenchmarkRuntimeClassColdStart -benchtime=5x \
  ./test/e2e/extensions/... -timeout 10m

# Warm claim benchmark (3 iterations per pool size)
SANDBOX_RUNTIME_CLASS=default \
  go test -v -run='^$' -bench=BenchmarkRuntimeClassWarmClaim -benchtime=3x \
  ./test/e2e/extensions/... -timeout 10m
```

### gVisor

```shell
SANDBOX_RUNTIME_CLASS=gvisor \
  SANDBOX_POOL_SIZES=4,8,12,16,20,24 \
  go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 30m
```

### Kata

Kata VMs consume ~250m CPU + 350Mi RAM each (pod overhead from the RuntimeClass).
The test auto-detects cluster CPU capacity and skips pool sizes that exceed it.

```shell
SANDBOX_RUNTIME_CLASS=kata \
  SANDBOX_POOL_SIZES=4,6,8,12,16 \
  go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 60m
```

## Batch Sizing

`TestRuntimeClassBurstRecovery` fires claims in batches to simulate sustained
load. The batch size is computed dynamically:

```
batchSize = min(max(4, poolSize / 2), 8)
```

- Pool 4 → batch 4
- Pool 8 → batch 4
- Pool 12 → batch 6
- Pool 16 → batch 8 (cap)
- Pool 24 → batch 8 (cap)

Batches fire with a 100ms settle interval between them. The test stops when
`ReadyReplicas ≤ 1` (pool depleted) or after `2 × poolSize` total claims.

## Calibration

Before the pool-size loop, a one-time calibration phase runs:

1. Creates a calibration pool (size = `max(4, workers×2)`, capped by CPU for VM runtimes).
2. Claims a single sandbox to measure **warm baseline** — the irreducible
   create-claim-watch latency.
3. Drains and refills the full pool to measure **batch refill rate** and
   **refill per slot**.

These values determine the warm/cold threshold (fixed at **1 second** — the
customer-experience boundary) and quality zone boundaries.

## Reading Results

### CSV columns

```
batch,claim,latency_sec,type,wall_offset_sec,ready_at_start
```

| Column | Description |
|--------|-------------|
| `batch` | Batch number (1-based) |
| `claim` | Claim index within the batch |
| `latency_sec` | Time from claim creation to Ready condition |
| `type` | `warm` (< 1s) or `cold` (>= 1s) |
| `wall_offset_sec` | Seconds since the test started |
| `ready_at_start` | Pool ReadyReplicas when this batch fired |

### CSV header and footer

The file starts with `# key,value` metadata lines (cluster ID, instance type,
runtime class, calibration results) and ends with summary stats:

```
# total_batches,6
# total_claims,48
# warm_claims,48
# cold_claims,0
# green_claims,21
# grey_zone_claims,27
# worst_start_sec,0.752
# over_cold_claims,2
# total_duration_sec,4.795
# throughput_claims_per_sec,10.0
```

### Quality zones

Claims are classified into quality zones based on latency:

| Zone | Range | Meaning |
|------|-------|---------|
| **Green** | ≤ warm_baseline × 1.2 | Optimal — indistinguishable from a single warm claim |
| **Grey** | warm_baseline × 1.2 … 1s | Elevated latency from reconciler serialization, still warm |
| **Cold** | > 1s | Cold start territory — pool was exhausted |
| **Over-cold** | > batch_refill time | Worse than a full pool refill cycle |

The grey zone is caused by controller work-queue serialization (~160ms cycle),
etcd write contention, and watch event coalescing — it is runtime-independent.

## Report Directory Structure

CSV files are written to an auto-constructed subdirectory:

```
<cluster_id>_<instance_type>_<date>_<runtime_class>/
  burst_recovery_<runtime>_pool4.csv
  burst_recovery_<runtime>_pool8.csv
  burst_recovery_<runtime>_pool16.csv
  ...
```

Example: `vvoron420gcp22-hjmvw-worker_n2-standard-8_20260722_default/`

If the directory already exists, a numeric suffix is appended (`_2`, `_3`, ...).

## Slides

A Marp-compatible slide deck with benchmark findings, architecture diagrams,
and analysis is available at:

```
test/e2e/extensions/warm-pool-benchmark-slides.md
```

Render with `marp warm-pool-benchmark-slides.md` or open in VS Code with the
Marp extension.
