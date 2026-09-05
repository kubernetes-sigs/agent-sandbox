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
| `SANDBOX_POOL_SIZES` | `{cpus/2, cpus, cpus×2}` | Comma-separated pool sizes for burst recovery and warm claim benchmarks. Defaults to half, full, and double the cluster's total worker CPU capacity. |
| `SANDBOX_BATCH_CAP` | `10` | Maximum number of claims fired per batch in burst recovery. Lower values reduce controller serialization; higher values stress the reconcile loop. |
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
The test auto-detects cluster CPU capacity and skips pool sizes exceeding 300%.

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

In regular (non-longevity) mode, the delay defaults to 100ms. The test
always fires `2×poolSize` claims to capture the warm-to-cold transition
point. Pool depletion (`ReadyReplicas ≤ 1`) is logged but does not stop
the test — claims past depletion exercise the cold fallback path and
appear as `is_warm=false` in the CSV.

## Pool Lifecycle and Fill Measurement

`TestRuntimeClassBurstRecovery` creates a fresh pool for each pool size to
avoid stale controller state (expectations tracker, observedGeneration) from
degrading fill times across iterations. The flow:

1. **Calibration**: a temporary pool at 4 replicas (capped by CPU for VM
   runtimes) measures **warm baseline** — the irreducible create-claim-watch
   latency. The calibration pool and claim are deleted before burst iterations.
2. **Per pool size**: a new pool is created at target replicas, **fill time**
   is measured (time for `ReadyReplicas` to reach target), burst claims run,
   then the pool and all claims are deleted. In longevity mode, `pool_fill_sec`
   measures time until `minReady` (roughly `2×batchSize`) rather than full
   target replicas — the pool continues filling in the background while claims
   fire, so this metric is not comparable to full-fill times in regular mode.
   Between iterations, the test requires all pods in the namespace (including
   Terminating) to be fully gone before proceeding — this ensures the next
   pool size starts with all CPU capacity available, which is critical for kata
   where pod termination can take 5-10 seconds per VM. Cleanup failures
   (claim deletion, pod drain) fail the test to prevent invalid measurements.

VM runtimes skip pool sizes that exceed **300%** of worker CPU capacity.
Overprovisioning works well for kata — scheduler queues VMs while the
pool maintains a larger buffer of pre-warmed slots. Empirically, green
ratio and throughput both improve past 100% CPU (e.g., pool-28 on a
3×8 vCPU cluster yields 89% green at 3.3 claims/s vs 65% at 2.3 for
pool-16).

Fill time accounts for the controller's `slowStartBatch` exponential ramp
(1, 2, 4, 8… concurrent creates) and is used to derive claim timeouts for
that specific pool size. The green threshold is fixed at **1 second** — claims
above this are classified as grey (warm but slow) or cold (fallback).

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
# kubernetes_version,v1.32.1
# sandbox_version,0.5.3
# provider,gce
# worker_count,3
# total_cpu_capacity,24
# total_ram_capacity_bytes,100663296000
# preexisting_pods,12
# allocated_cpu_millis,3500
# allocated_ram_bytes,4294967296
# instance_type,n2-standard-8
# runtime_class,kata
# pool_size,8
# workload_sec,30
# cold_baseline_sec,8.500
# warm_baseline_sec,0.350
# green_threshold_sec,1.000
# pool_fill_sec,12.500
# batch_size,4
# max_claims,16
# inter_batch_delay_ms,100
```

Footer summary:

```text
# total_batches,4
# total_claims,16
# green_claims,8
# grey_claims,4
# cold_claims,4
# worst_start_sec,9.215
# total_duration_sec,18.450
# time_to_all_ready_sec,15.720
# throughput_claims_per_sec,0.9
# pods_on_node,worker-1,6
# pods_on_node,worker-2,5
# pods_on_node,worker-3,5
```

### Quality zones

Claims are classified into three zones using the controller's `is_warm` signal
and a 1-second latency threshold:

| Zone | Criteria | Meaning |
|------|----------|---------|
| **Green** | `is_warm=true` and ≤ 1s | Sub-second from warm pool — target SLA met |
| **Grey** | `is_warm=true` and > 1s | Warm-served but slow — scheduling or adoption lag |
| **Cold** | `is_warm=false` | Pool failed to serve the claim — cold fallback path |

`is_warm` is ground truth from the controller: `true` when the sandbox pod
existed before the claim was created (pre-warmed), `false` when the controller
created a new sandbox on demand. This avoids runtime-specific latency
thresholds — cold start varies from ~2s (runc) to ~8s (kata), but `is_warm`
is unambiguous regardless of runtime.

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
  10 batches with aggregated p50/p95 latencies, throughput, green/grey/cold
  counts, and batch size direction for live monitoring.

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

### Functional E2E Coverage

- **Volume and initContainer injection**: Validate initContainer + emptyDir
  execution patterns and file-proxy (virtiofsd/gofer) mount initialization
  across sandboxed runtimes. The kata image volume gap
  ([kata-containers#13749](https://github.com/kata-containers/kata-containers/issues/13749))
  was found via openshell, not direct-driver tests — these paths need explicit
  coverage.
- **Warm pool adoption integrity**: Verify that controller state transitions
  (Pending → Ready → Claimed) and metadata mutations apply cleanly without
  triggering guest container restarts or corrupting runtime state.
- **Network proxy reachability**: Confirm end-to-end ingress routing through
  the sandbox-router into Kata virtual TAP interfaces and gVisor networking
  stacks.
- **Virtiofs storage I/O validation**: Verify file-based tool calls (writes,
  reads, workspace updates) function correctly over virtiofs rather than
  overlayfs. Perform rapid multi-file agent workspace I/O inside mounted
  volumes and check file consistency across the guest-host boundary — stale
  reads, partial writes, and metadata desync are common virtiofs failure
  modes under concurrent access. Requires bare metal for accurate I/O
  throughput profiling.

### Infrastructure & Test Harness

- **RuntimeClass auto-detection**: Query installed RuntimeClasses from the cluster
  to drive multi-runtime test sweeps without manual `SANDBOX_RUNTIME_CLASS` env
  var. Not all nodes support all runtimes (e.g., `kata-nvidia-gpu` requires
  specific node capabilities).
- **Multi-size lifecycle subtests**: Run `TestRuntimeClassLifecycle` at small (2)
  and half-CPU pool sizes to validate the fill → claim → refill cycle under
  moderate scheduling pressure in CI.

### Cluster Health Observability

The CSV header now captures static cluster state (total RAM, preexisting pods,
allocated CPU/RAM, provider, versions) and the footer includes per-node pod
counts to surface scheduling skew. The remaining items add *dynamic*
node-level context — what the cluster was doing *during* the run. A 9s cold
start at 30% node CPU means something very different from one at 95%.

- **Node resource snapshots**: Sample `kubectl top nodes` (or metrics-server
  API) at the start and end of each pool iteration, and periodically during
  longevity runs. Write per-node CPU% and memory usage to a separate
  `node_health_<runtime>_pool<N>.csv`. Key aggregates: average and peak
  CPU%, average and peak memory%.
- **Node condition monitoring**: Poll node conditions (MemoryPressure,
  DiskPressure, PIDPressure) during the test. Flag iterations where any
  worker entered a pressure condition — these results are unreliable for
  benchmarking and should be marked in the CSV.
- **Runtime daemon health**: For kata, check hypervisor memory overhead
  (~256MB/VM) and file descriptor counts on runtime proxy daemons
  (`kata-shim-v2`, `virtiofsd`). For gVisor, check `runsc` sentry memory.
  High FD counts or OOM kills on the daemon side cause cold starts that look
  like controller slowness in the CSV.

### Time-to-Ready Breakdown

The current milestone tracker measures coarse phases (create-ack, adoption,
schedule, runtime, propagate) but does not decompose the runtime phase into
its constituent steps. For openshell-style workloads the full stack is:

1. **Hypervisor / VM boot** — firmware load → kernel → kata-agent ready.
   Varies significantly between bare-metal KVM (~1.5s) and nested virt
   (~4-8s). Requires bare metal for accurate cold-start SLA measurement.
   The test should detect nested virt and annotate results.
2. **Virtiofs volume mount** — time for virtiofsd to establish the shared
   filesystem between host and guest. Blocks container start if workspace
   volumes are mounted. Requires bare metal for accurate I/O profiling —
   double-virtualized storage layers inflate latency.
3. **Supervisor and L7 proxy startup** — sandbox-router, sidecar proxies,
   and any injected init containers that must be ready before the agent
   can accept tool calls.
4. **Agent process execution readiness** — time from container start to
   the agent process accepting its first request (health check passing).

Exposing these sub-phases in the CSV (as optional columns when the runtime
supports it) would make it possible to pinpoint whether a regression is in
the hypervisor layer, the storage layer, or the application stack.

### Controller & Runtime Observability

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

### Scale & Stress Coverage (Nightly/Non-Blocking)

- **Replenishment latency under burst claims**: Ensure pool controllers
  gracefully queue concurrent claim bursts and absorb microVM cold-start
  boot latency (1-3s) without claim timeouts. Currently covered by
  `TestRuntimeClassBurstRecovery` but only as a manual benchmark — needs
  a nightly CI job with pass/fail thresholds on green ratio and p95 latency.
- **Host density and resource overhead**: Stress-test node density limits
  to monitor hypervisor memory overhead (~256MB/VM) and file descriptor
  limits on runtime proxy daemons. Identify the point where adding more
  pool replicas degrades rather than improves claim latency.
- **Long-term stability and drift (7-day soak)**: Run continuous
  claim-and-release loops over an extended duration to detect resource
  leaks and zombie processes under sustained warm pool churn. Assert zero
  memory growth in sandbox-router, complete finalization of hypervisor
  processes (qemu/cloud-hypervisor), and no orphaned virtiofsd daemons on
  host nodes. The existing longevity mode (`SANDBOX_LONGEVITY`) caps at a
  few hours — a week-long soak needs periodic node-side auditing (process
  counts, FD counts, RSS snapshots) written to a time-series CSV.
- **Gateway and node saturation thresholds**: Progressively scale active
  Kata sandboxes on a single node until CPU/memory saturation or gateway
  throughput limits are hit. Identifies maximum sandbox density under
  dual-layer overhead (hypervisor + sandbox-router) and establishes
  baseline density guidance for platform sizing. Record the inflection
  point where claim latency degrades and the failure mode (OOM kill,
  throttling, gateway timeout). Requires bare metal for accurate density
  limits — nested virt overhead skews the saturation point.

## Design Decisions

All claims use `ShutdownPolicy: Delete` with `TTLSecondsAfterFinished: 0`
(configurable via `SANDBOX_TTL`) to prevent zombie claim/sandbox/pod
accumulation. Without this, the API default (`Retain` or no lifecycle at all)
leaves finished claims and their underlying VMs alive indefinitely — a
resource leak and security gap documented in
[#1306](https://github.com/kubernetes-sigs/agent-sandbox/issues/1306).
