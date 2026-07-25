# Runtime Class Benchmark Tests

Runtime-class-aware e2e tests and benchmarks for `sigs.k8s.io/agent-sandbox`.
They measure cold start latency, warm pool claim speed, and burst recovery
behaviour across different container runtimes (runc, gVisor, kata).

## Prerequisites

- A Kubernetes cluster with agent-sandbox deployed (CRDs + controller).
- `KUBECONFIG` pointing at the cluster.
- For gVisor tests: RuntimeClass `gvisor` installed.
  See [gVisor installation guide](https://gvisor.dev/docs/user_guide/install/).
- For kata tests: RuntimeClass `kata` installed. Requires nodes with hardware
  virtualization (`/dev/kvm`). See
  [Kata Containers installation guide](https://github.com/kata-containers/kata-containers/tree/main/docs/install).

## Tests and Benchmarks

| Name | Type | What it measures |
|------|------|------------------|
| `TestRuntimeClassLifecycle` | Test | Full SandboxTemplate → WarmPool → SandboxClaim lifecycle with a given RuntimeClass |
| `TestRuntimeClassStartupComparison` | Test | Cold start vs warm claim side-by-side, reports speedup ratio |
| `TestRuntimeClassBurstRecovery` | Test | Sustained batch load against various pool sizes, writes per-claim CSV reports with quality zone stats |
| `BenchmarkRuntimeClassColdStart` | Benchmark | Raw cold sandbox creation latency per image (`sandbox-ready-sec/op`, `worst-sec` metrics) |
| `BenchmarkRuntimeClassWarmClaim` | Benchmark | Warm pool claim latency across image × pool-size combinations (`claim-ready-sec/op`, `worst-sec` metrics) |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_RUNTIME_CLASS` | *(required)* | RuntimeClass name: `default` (cluster default / runc), `gvisor`, `kata`, etc. Tests skip when unset. |
| `SANDBOX_POOL_SIZES` | total worker CPUs | Comma-separated pool sizes for burst recovery and warm claim benchmarks. Defaults to the cluster's total worker CPU count when unset. |
| `SANDBOX_BATCH_CAP` | `10` | Maximum number of claims fired per batch in burst recovery. Lower values reduce controller serialization; higher values stress the reconcile loop. |
| `SANDBOX_SETTLE_SEC` | `2` | Seconds to wait after pool fill before starting burst claims. Lets the controller work queue drain so fill-residue doesn't inflate batch 1 latencies. Set to `0` to measure raw post-fill behavior. |
| `SANDBOX_REPORT_DIR` | `.` (cwd) | Base directory for CSV output. A subdirectory is auto-created per run. |
| `SANDBOX_CLUSTER_ID` | *(auto-detected)* | Override cluster identity string in report paths |
| `SANDBOX_VERSION` | *(auto-detected)* | Override agent-sandbox version. Defaults to the controller deployment image tag. |
| `SANDBOX_WORKLOAD_SEC` | `30` | Seconds the workload container sleeps in burst recovery and benchmark tests. `0` uses a pause container. |
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

```text
batchSize = min(max(4, poolSize / 2), batchCap)
```

The batch cap defaults to 10 (`SANDBOX_BATCH_CAP`).

- Pool 4 → batch 4
- Pool 8 → batch 4
- Pool 12 → batch 6
- Pool 16 → batch 8
- Pool 20 → batch 10 (cap)
- Pool 32 → batch 10 (cap)

Batches fire with a 100ms settle interval between them. The test stops when
`ReadyReplicas ≤ 1` **and** at least `poolSize` claims have been issued
(ensuring at least one full pass through the pool), or after `2 × poolSize`
total claims — whichever comes first.

## Pool Reuse and Fill Measurement

`TestRuntimeClassBurstRecovery` creates a single namespace, template, and pool
that are reused across all pool sizes. The flow:

1. **Calibration**: pool is created at 4 replicas (capped by CPU for VM
   runtimes). A single claim measures **warm baseline** — the irreducible
   create-claim-watch latency.
2. **Scale to 0**: pool is drained, calibration claim deleted.
3. **Per pool size**: scale pool to target replicas, measure **fill time**
   (time for `ReadyReplicas` to reach target), run burst claims, scale back
   to 0. A 2-second cooldown separates iterations.

Fill time accounts for the controller's `slowStartBatch` exponential ramp
(1, 2, 4, 8… concurrent creates) and is used to derive claim timeouts for
that specific pool size. The warm/cold threshold is fixed at **1 second**.

## Reading Results

### CSV columns

```text
batch,claim,latency_sec,type,wall_offset_sec,ready_at_start,create_ack_ms,adoption_ms,schedule_ms,runtime_ms,propagate_ms,e2e_ms,is_warm
```

| Column | Description |
|--------|-------------|
| `batch` | Batch number (1-based) |
| `claim` | Claim index within the batch |
| `latency_sec` | Time from claim creation to Ready condition |
| `type` | `warm` (< 1s) or `cold` (>= 1s) |
| `wall_offset_sec` | Seconds since the test started |
| `ready_at_start` | Pool ReadyReplicas when this batch fired |
| `create_ack_ms` | API server round-trip: create call to return |
| `adoption_ms` | Controller bind time: create returned to sandbox name set |
| `schedule_ms` | Pod scheduling: pod created to PodScheduled condition |
| `runtime_ms` | Container runtime: PodScheduled to PodReady |
| `propagate_ms` | Status propagation: sandbox Ready to claim Ready |
| `e2e_ms` | End-to-end: create call to claim Ready |
| `is_warm` | Whether the pod existed before the claim (pre-warmed) |

### CSV header and footer

The file starts with `# key,value` metadata lines (cluster ID, instance type,
runtime class, pool fill time) and ends with summary stats:

```text
# total_batches,6
# total_claims,48
# warm_claims,48
# cold_claims,0
# green_claims,21
# grey_zone_claims,27
# worst_start_sec,0.752
# over_cold_claims,2
# time_to_all_ready_sec,3.214
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
| **Over-cold** | > pool fill time | Worse than the measured pool fill time |

The grey zone is caused by controller work-queue serialization (~160ms cycle),
etcd write contention, and watch event coalescing — it is runtime-independent.

## Report Directory Structure

CSV files are written to an auto-constructed subdirectory:

```text
<cluster_id>_<instance_type>_<date>_<runtime_class>/
  burst_recovery_<runtime>_pool4.csv
  burst_recovery_<runtime>_pool8.csv
  burst_recovery_<runtime>_pool16.csv
  ...
```

Example: `vvoron420gcp22-hjmvw-worker_n2-standard-8_20260722_default/`

If the directory already exists, a numeric suffix is appended (`_2`, `_3`, ...).

## Roadmap

- **Split functional vs stress tests**: Separate `runtime_class_test.go` into
  functional tests (CI-suitable gating) and stress/benchmarks (hardware-dependent,
  CSV-producing). Enables a dedicated CI job for runtime-aware e2e validation
  without benchmark noise.
- **RuntimeClass auto-detection**: Query installed RuntimeClasses from the cluster
  to drive multi-runtime test sweeps without manual `SANDBOX_RUNTIME_CLASS` env
  var. Not all nodes support all runtimes (e.g., `kata-nvidia-gpu` requires
  specific node capabilities).
- **Virtualization topology reporting**: Detect whether worker nodes are bare-metal
  with hardware virtualization or virtual with nested virtualization. Relevant for
  kata cold start analysis — nested virt adds measurable overhead.
- **CPU-relative benchmark pool sizes**: Default `SANDBOX_POOL_SIZES` to
  `{cpuCapacity/2, cpuCapacity, cpuCapacity*2}` — half (comfortable headroom),
  full (capacity cliff), and double (forced cold starts to measure the penalty).
- **Multi-size lifecycle subtests**: Run `TestRuntimeClassLifecycle` at small (2)
  and half-CPU pool sizes to validate the fill → claim → refill cycle under
  moderate scheduling pressure in CI.
- **Probe-based settle detection**: Replace the fixed `SANDBOX_SETTLE_SEC` delay
  with a single probe claim after pool fill. If the probe latency falls within
  the green threshold, the controller work queue is empirically drained and burst
  can start immediately. If not, back off and retry. Eliminates both the risk of
  starting too early (inflated baselines) and waiting too long (wasted time on
  fast clusters).

