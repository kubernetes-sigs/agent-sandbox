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
| `SANDBOX_LONGEVITY` | *(unset)* | Go duration (e.g. `2h`, `30m`) to run burst recovery in longevity mode: continuous batches with adaptive sizing until the deadline. |
| `SANDBOX_DEBUG` | *(unset)* | Set to any non-empty value to dump scoped controller logs after each pool iteration even on success. |
| `SANDBOX_REPORT_DIR` | `artifacts` | Base directory for CSV output and controller logs. A subdirectory is auto-created per run. |
| `SANDBOX_CLUSTER_ID` | *(auto-detected)* | Override cluster identity string in report paths |
| `SANDBOX_VERSION` | *(auto-detected)* | Override agent-sandbox version. Defaults to the controller deployment image tag. |
| `SANDBOX_WORKLOAD_SEC` | `30` | Seconds the workload container sleeps in burst recovery and benchmark tests. `0` uses a pause container. Longevity mode overrides this to `max(10, coldStart×5)` unless explicitly set — derived from cold start calibration so pods survive pool fill time. |
| `SANDBOX_TTL` | `0` | TTL in seconds for claim auto-cleanup after workload finishes. All claims use `ShutdownPolicy: Delete` with this TTL. Set higher to simulate Retain-like behavior where claims linger before deletion. |
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

# Longevity soak test — 2 hours of sustained batch claims against a single pool
SANDBOX_RUNTIME_CLASS=kata-clh \
  SANDBOX_POOL_SIZES=35 \
  SANDBOX_LONGEVITY=2h \
  go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 3h

# Quick debug run with controller log dump on success
SANDBOX_RUNTIME_CLASS=kata-clh \
  SANDBOX_POOL_SIZES=35 \
  SANDBOX_LONGEVITY=5m \
  SANDBOX_DEBUG=true \
  go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 10m
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

In longevity mode (`SANDBOX_LONGEVITY`), the initial batch size is computed
from the cold start baseline to avoid depleting the pool:

```text
batchSize = max(4, int(0.3 × poolSize / coldStartSec))
```

The `0.3` safety factor ensures roughly 3× more refill capacity than drain
rate. Adaptive sizing then adjusts ±1 per batch: decrease when
`ready < poolSize/2`, increase when `ready > poolSize - batchSize`.

The inter-batch delay is also computed from the cold start baseline:

```text
delay = coldStartSec × batchSize / poolSize   (floor 50ms)
```

This is static (computed once) to avoid fighting with adaptive batch sizing.
For runc this yields ~50ms, for kata ~500ms.

In regular (non-longevity) mode, the delay defaults to 100ms. The test stops
when `ReadyReplicas ≤ 1` **and** at least `poolSize` claims have been issued
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
   to 0. Between iterations, the test polls until all pods in the namespace
   (including Terminating) are fully gone — this ensures the next pool size
   starts with all CPU capacity available, which is critical for kata where
   pod termination can take 5-10 seconds per VM.

VM runtimes skip pool sizes that exceed **300%** of worker CPU capacity.
Overprovisioning works well for kata — scheduler queues VMs while the
pool maintains a larger buffer of pre-warmed slots. Empirically, warm
ratio and throughput both improve past 100% CPU (e.g., pool-28 on a
3×8 vCPU cluster yields 89% warm at 3.3 claims/s vs 65% at 2.3 for
pool-16).

Fill time accounts for the controller's `slowStartBatch` exponential ramp
(1, 2, 4, 8… concurrent creates) and is used to derive claim timeouts for
that specific pool size. The warm/cold threshold is fixed at **1 second**.

## Reading Results

### CSV columns

```text
batch,claim,batch_size,latency_sec,timestamp,wall_offset_sec,ready_at_start,create_ack_ms,adoption_ms,schedule_ms,runtime_ms,propagate_ms,e2e_ms,is_warm
```

| Column | Description |
|--------|-------------|
| `batch` | Batch number (1-based) |
| `claim` | Claim index within the batch |
| `batch_size` | Batch size used for this batch (may vary with adaptive sizing) |
| `latency_sec` | Time from claim creation to Ready condition |
| `timestamp` | Claim creation time in RFC3339 UTC (matches controller log format for cross-referencing) |
| `wall_offset_sec` | Seconds since the test started |
| `ready_at_start` | Pool ReadyReplicas when this batch fired |
| `create_ack_ms` | API server round-trip: create call to return |
| `adoption_ms` | Controller bind time: create returned to sandbox name set |
| `schedule_ms` | Pod scheduling: pod created to PodScheduled (warm: during pool fill; cold: during claim) |
| `runtime_ms` | Container runtime: PodScheduled to PodReady (warm: VM boot / container start during pool fill; cold: during claim) |
| `propagate_ms` | Status propagation: sandbox Ready to claim Ready |
| `e2e_ms` | End-to-end: create call to claim Ready |
| `is_warm` | Whether the pod existed before the claim (pre-warmed) |

### CSV header and footer

The file starts with `# key,value` metadata lines and ends with summary stats.

Header metadata:

```text
# cluster_id,my-cluster-abcde
# worker_count,3
# total_cpu_capacity,24
# instance_type,n2-standard-8
# runtime_class,kata
# pool_size,8
# workload_sec,0
# warm_baseline_sec,0.350
# warm_cold_threshold_sec,1.000
# pool_fill_sec,12.500
# batch_size,4
# max_claims,16
# settle_sec,2
# inter_batch_delay_ms,100
```

Footer summary:

```text
# total_batches,6
# total_claims,48
# under_1s_claims,48
# over_1s_claims,0
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
| **Green** | ≤ 500ms | Invisible to the caller — warm pool delivered its promise |
| **Grey** | 500ms … 1s | Contention-degraded but still faster than cold start |
| **Cold** | > 1s | Warm pool failed to mask the cold start |
| **Over-cold** | > pool fill time | Worse than the measured pool fill time |

The grey zone is dominated by API server round-trip (~170ms) and controller
adoption overhead (~100-250ms) — it is runtime-independent.

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

## Longevity Mode

Set `SANDBOX_LONGEVITY` to a Go duration (e.g. `2h`, `30m`) to run
`TestRuntimeClassBurstRecovery` as a sustained soak test. Batches fire
continuously until the deadline with adaptive batch sizing that self-tunes
to the controller's refill rate.

Key differences from regular burst mode:

- **Heuristic initial batch**: `max(4, int(0.3 × poolSize / coldStartSec))`
  instead of `min(max(4, poolSize/2), batchCap)`. The cold start baseline
  drives the initial estimate so the pool isn't depleted immediately.
- **Adaptive sizing**: batch size decreases by 1 when `ready < poolSize/2`
  (pool under pressure) and increases by 1 when `ready > poolSize - batchSize`
  (pool recovered). The wide steady zone prevents oscillation.
- **Static inter-batch delay**: `coldStartSec × batchSize / poolSize` (floor
  50ms), computed once from the cold start baseline. Avoids fighting with
  the adaptive batch size.
- **Workload override**: unless `SANDBOX_WORKLOAD_SEC` is explicitly set,
  longevity mode sets workload duration to `max(10, coldStart×5)` seconds —
  derived from cold start calibration so pods survive one full pool fill
  cycle.
- **Claim auto-cleanup**: all claims (burst, baseline, lifecycle, benchmarks)
  use `ShutdownPolicy: Delete` with `TTLSecondsAfterFinished: 0` by default
  (configurable via `SANDBOX_TTL`). The controller deletes claims after the
  workload exits — no client-side GC needed. This prevents defer cleanup
  storms at test end (a 2-minute run produces 800+ claims).
- **Minimum pool size**: longevity mode skips pool sizes below 20 — smaller
  pools deplete too fast for meaningful adaptive tuning.
- **Summary CSV**: a `burst_summary_<runtime>_pool<N>.csv` is written every
  10 batches with aggregated p50/p95 latencies, throughput, warm ratio, and
  batch size direction for live monitoring.

## Controller Log Capture

After each pool size iteration, scoped controller logs are captured using
Kubernetes `SinceTime` filtering — only logs from the pool's test period are
fetched, not the entire pod lifetime.

- **Regular burst**: controller logs are dumped unconditionally after each
  pool iteration (the scoped period is short, so the cost is negligible).
- **Longevity mode**: controller logs are dumped only on test failure or when
  `SANDBOX_DEBUG` is set to any non-empty value.

Logs are saved as `controller-pool<N>-<podname>.log` (or
`controller-longevity-pool<N>-<podname>.log`) in the test artifacts directory.
The last 42 lines are also printed to test output. Claim timestamps in the CSV
use the same RFC3339 UTC format as the controller logs, enabling direct
cross-referencing between claim records and controller reconcile activity.

On test failure, the framework's built-in `afterEach` hook additionally dumps
the full (unscoped) controller log as a fallback.

## Roadmap

- **Split functional vs stress tests**: Separate `runtime_class_test.go` into
  functional tests (CI-suitable gating) and stress/benchmarks (hardware-dependent,
  CSV-producing). Enables a dedicated CI job for runtime-aware e2e validation
  without benchmark noise.
- **RuntimeClass auto-detection**: Query installed RuntimeClasses from the cluster
  to drive multi-runtime test sweeps without manual `SANDBOX_RUNTIME_CLASS` env
  var. Not all nodes support all runtimes (e.g., `kata-nvidia-gpu` requires
  specific node capabilities).
- **CPU-relative benchmark pool sizes**: Default `SANDBOX_POOL_SIZES` to
  `{cpuCapacity/2, cpuCapacity, cpuCapacity*2}` — half (comfortable headroom),
  full (capacity cliff), and double (forced cold starts to measure the penalty).
- **Multi-size lifecycle subtests**: Run `TestRuntimeClassLifecycle` at small (2)
  and half-CPU pool sizes to validate the fill → claim → refill cycle under
  moderate scheduling pressure in CI.
- **Probe-based settle detection**: Replace the fixed `SANDBOX_SETTLE_SEC` delay
  with a single probe claim after pool fill. If the probe latency falls within
  the green threshold (500ms), the controller work queue is empirically drained and burst
  can start immediately. If not, back off and retry. Eliminates both the risk of
  starting too early (inflated baselines) and waiting too long (wasted time on
  fast clusters).
- **Per-pool metrics capture**: Scrape the controller's Prometheus endpoint
  (`/metrics` on port 8080) before and after each pool iteration. Save the
  delta as `metrics_<runtime>_pool<N>.prom` in the results directory. Key
  metrics to capture, in priority order:
  1. **Controller**: `controller_runtime_reconcile_total` (by result),
     `controller_runtime_reconcile_time_seconds` (reconcile duration histogram),
     `workqueue_depth` and `workqueue_unfinished_work_seconds` (worker
     saturation — if queue depth stays >0 during burst, workers are the
     ceiling).
  2. **API server**: `apiserver_request_duration_seconds` filtered to
     `resource=sandboxes,sandboxclaims,pods` (request latency),
     `apiserver_current_inflight_requests` (throttling).
  3. **etcd**: `etcd_request_duration_seconds` for `type=put` (raw write
     latency), `etcd_disk_wal_fsync_duration_seconds` (disk bottleneck).
  4. **Kubelet**: `kubelet_pod_start_duration_seconds` (node-side pod startup,
     most useful for kata VM boot breakdown).
- **Kata VM boot tracing**: Enable kata's built-in Jaeger tracing by setting
  `enable_tracing = true` and `jaeger_endpoint` in
  `/etc/kata-containers/configuration.toml` before the test run. This exposes
  microsecond-level spans for the full VM boot sequence: firmware load →
  kernel boot → kata-agent start → rootfs mount → container exec. CRI-O logs
  only show shim fork + network setup (~400ms); the remaining ~8s of kata cold
  start is invisible without tracing. Alternatively, scrape the per-sandbox
  shim-monitor.sock metrics endpoint while VMs are still running to capture
  boot timing without Jaeger infrastructure.

## Design Decisions

All claims use `ShutdownPolicy: Delete` with `TTLSecondsAfterFinished: 0`
(configurable via `SANDBOX_TTL`) to prevent zombie claim/sandbox/pod
accumulation. Without this, the API default (`Retain` or no lifecycle at all)
leaves finished claims and their underlying VMs alive indefinitely — a
resource leak and security gap documented in
[#1306](https://github.com/kubernetes-sigs/agent-sandbox/issues/1306).
