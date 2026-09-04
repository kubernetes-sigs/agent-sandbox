---
title: "Performance Assessment"
linkTitle: "Performance Assessment"
weight: 33
description: >
  How to measure, benchmark, and tune the performance of the Agent Sandbox controller.
---

This guide covers the tools and techniques available for assessing the performance of Agent Sandbox — from tuning controller concurrency, to running load tests, to collecting and interpreting benchmark results.

## Controller Performance Tuning

The `agent-sandbox-controller` exposes several flags that directly affect throughput and API server pressure. Raising these is the first step before running any load test. The table below is kept in sync with [`docs/configuration.md`](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/configuration.md), which is the canonical flag reference — check there first if the two ever disagree.

| Flag | Default | Description |
|------|---------|-------------|
| `--sandbox-concurrent-workers` | `100` | Max concurrent reconciles for the Sandbox controller |
| `--sandbox-claim-concurrent-workers` | `50` | Max concurrent reconciles for the SandboxClaim controller |
| `--sandbox-warm-pool-concurrent-workers` | `1` | Max concurrent reconciles for the SandboxWarmPool controller |
| `--sandbox-template-concurrent-workers` | `1` | Max concurrent reconciles for the SandboxTemplate controller |
| `--sandbox-warm-pool-max-batch-size` | `300` | Max sandboxes the SandboxWarmPool controller creates or deletes in a single batch |
| `--kube-api-qps` | `-1` (no client-side throttling) | Disables client-side rate limiting to the Kubernetes API server. Server-side throttling (API Priority and Fairness) still applies. When setting a positive value, use at least the sum of all `--*-concurrent-workers` flags to avoid starving reconcile loops. |
| `--kube-api-burst` | `10` | Max burst for API server throttle requests. Ignored when `--kube-api-qps` is `-1`. When `--kube-api-qps` is set to a positive value, set this to equal or greater than `--kube-api-qps`. Must stay a positive integer even when unused — the controller exits on startup if it's `<= 0`. |

### Choosing worker counts

Each `--*-concurrent-workers` flag controls the number of independent goroutines the corresponding controller runs. For example, setting `--sandbox-claim-concurrent-workers=10` allows the SandboxClaim controller to process up to 10 SandboxClaims in parallel.

A good starting point is to size each value to the expected steady-state object count for that type, or to a calculated fraction of your maximum burst load. A cluster with a single WarmPool only needs `--sandbox-warm-pool-concurrent-workers=1`.

**Important caveat:** increasing worker counts only improves throughput when the controller itself is the bottleneck. If the bottleneck is the kube-apiserver or the container runtime (e.g., slow pod startup), adding more workers can actually *increase* latency by creating additional API server contention. Use the metrics in the [Metrics Collected](#metrics-collected) section and APF dashboards to confirm where the bottleneck lies before tuning these values.

### Applying Flags

**Via manifest** — edit the relevant manifest for your deployment. The snippets below show the in-repo source under `k8s/`; the corresponding release assets (`sandbox.yaml`, `extensions.yaml`) carry the same `args`, but with the `ko://` placeholder replaced by a published `registry.k8s.io/agent-sandbox/agent-sandbox-controller:<version>` image.

*Core install (`k8s/controller.yaml`):*

```yaml
containers:
- name: agent-sandbox-controller
  image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller
  args:
  - --leader-elect=true
  - --sandbox-concurrent-workers=10
  - --sandbox-claim-concurrent-workers=10
  - --sandbox-warm-pool-concurrent-workers=10
  - --kube-api-qps=50
  - --kube-api-burst=100
```

*Extensions install (`k8s/extensions.controller.yaml`) — note the additional `--extensions` flag, which enables the SandboxTemplate controller and its associated RBAC. (`k8s/extensions.yaml` is RBAC only — the Deployment lives in `k8s/extensions.controller.yaml`, a second copy of the core Deployment applied after `sandbox.yaml` so the sequential `sandbox.yaml` → `extensions.yaml` install path lands a Deployment with `--extensions` set. Kustomize consumers get the same result via the patch in `k8s/kustomization.yaml`, which excludes this file.):*

```yaml
containers:
- name: agent-sandbox-controller
  image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller
  args:
  - --leader-elect=true
  - --extensions
  - --sandbox-concurrent-workers=10
  - --sandbox-claim-concurrent-workers=10
  - --sandbox-warm-pool-concurrent-workers=10
  - --sandbox-template-concurrent-workers=10
  - --kube-api-qps=50
  - --kube-api-burst=100
```

**Via `kubectl patch`** on a live cluster:

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type='json' \
  -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-claim-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-template-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kube-api-qps=50"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kube-api-burst=100"}
  ]'
```

**Via Kustomize** — use a JSON 6902 patch to append flags without replacing the existing `args` list. A strategic-merge patch on `containers[].args` replaces the entire list, which silently drops required flags such as `--leader-elect` and `--extensions`.

> **Note:** If you consume `k8s/kustomization.yaml` (or its rendered `sandbox-with-extensions.yaml` release asset) rather than applying `sandbox.yaml`/`extensions.yaml` sequentially, start from that file — it already assembles core + extensions into a single Deployment. Add the JSON 6902 patch below as an additional entry under its `patches:` list.

*Core install:*

```yaml
# patch-args.yaml
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-claim-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-warm-pool-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--kube-api-qps=50"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--kube-api-burst=100"
```

*Extensions install — also tune `--sandbox-template-concurrent-workers`:*

```yaml
# patch-args.yaml
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-claim-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-warm-pool-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--sandbox-template-concurrent-workers=10"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--kube-api-qps=50"
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: "--kube-api-burst=100"
```

Reference the patch from `kustomization.yaml` using `patches` with an explicit `target`:

```yaml
# kustomization.yaml
patches:
  - path: patch-args.yaml
    target:
      kind: Deployment
      name: agent-sandbox-controller
      namespace: agent-sandbox-system
```

---

## E2E Benchmarks

The repository ships Go benchmarks in `test/e2e/` that measure Sandbox and SandboxClaim startup latency against a live cluster.

### Available benchmarks

| Benchmark | File | What it measures |
|-----------|------|-----------------|
| `BenchmarkChromeSandboxStartup` | `chromesandbox_test.go` | Chrome Sandbox pod startup latency |
| `BenchmarkChromeSandboxClaimStartup` | `chromesandbox_claim_test.go` | Chrome SandboxClaim end-to-end startup latency |
| `BenchmarkWarmPoolParallelClaim` | `warmpool_benchmark_test.go` | Parallel SandboxClaim latency against a pre-warmed WarmPool |

### Running benchmarks

Run all e2e benchmarks:

```bash
make test-e2e-benchmarks
```

Or target a specific benchmark directly with `go test`:

```bash
go test -bench=BenchmarkChromeSandboxClaimStartup -benchtime=10x ./test/e2e/...
```

---

## Load Testing with ClusterLoader2

For scale testing, Agent Sandbox uses [ClusterLoader2](https://github.com/kubernetes/perf-tests/tree/master/clusterloader2) (CL2), the same framework used for Kubernetes scalability testing.

### Prerequisites

- A running Kubernetes cluster with the Agent Sandbox controller and CRDs installed.
- Go toolchain.
- The `perf-tests` repository cloned as a sibling to `agent-sandbox`:

```text
workspace/
├── agent-sandbox/
│   └── dev/
│       └── load-test/
└── perf-tests/
    └── clusterloader2/
```

### Basic startup latency test

This test creates a set of Sandboxes and measures their startup latency.

**1. Build ClusterLoader2** (run from `perf-tests/clusterloader2/`):

```bash
go build -o clusterloader2 ./cmd/clusterloader.go
```

**2. Run the test:**

```bash
# Against a GKE cluster
./clusterloader2 \
  --testconfig=../../agent-sandbox/dev/load-test/agent-sandbox-load-test.yaml \
  --kubeconfig=$HOME/.kube/config \
  --provider=gke

# Against a local kind cluster
./clusterloader2 \
  --testconfig=../../agent-sandbox/dev/load-test/agent-sandbox-load-test.yaml \
  --kubeconfig=$HOME/.kube/config \
  --provider=kind
```

**3. Verify results** — results are saved to `junit.xml` in the `clusterloader2/` directory:

```xml
<testsuite name="ClusterLoaderV2" tests="0" failures="0" errors="0" time="57.957">
  <testcase name="agent-sandbox-load-test: [step: 01] Start Startup Latency Measurement" .../>
  <testcase name="agent-sandbox-load-test: [step: 02] Create Sandboxes" .../>
  <testcase name="agent-sandbox-load-test: [step: 03] Wait for Sandboxes to be Ready" .../>
  <testcase name="agent-sandbox-load-test: [step: 04] Gather Results" .../>
  <testcase name="agent-sandbox-load-test: [step: 05] Delete Sandboxes" .../>
</testsuite>
```

---

## Test Recipes

`dev/load-test/test-recipes/` contains ready-made scenarios for more demanding performance testing. Some recipes query Prometheus directly for detailed latency metrics; others rely on ClusterLoader2's built-in measurement methods. See [Metrics Collected](#metrics-collected) for which recipe reports what.

### Available recipes

| Recipe | File | Purpose |
|--------|------|---------|
| Rapid burst | `rapid-burst-test.yaml` | Creates SandboxClaims in discrete high-rate bursts |
| High-volume ramp | `high-volume-test.yaml` | Ramps creation rate up then back down |
| Steady-state churn | `medium-scale-concurrent-load-test.yaml` | Measures sustained concurrent churn |
| Throughput | `throughput-test.yaml` | Measures raw creation throughput |
| Warm pool burst | `warmpool-burst-test.yaml` | Tests warm pool performance under burst load |

### Running the rapid burst test

The rapid burst test is the primary scalability scenario. It creates SandboxClaims in repeated bursts and records per-burst latency distributions.

```bash
cd dev/load-test/test-recipes
chmod +x run_rapid_burst.sh
./run_rapid_burst.sh                           # script defaults
BURST_SIZE=400 QPS=400 ./run_rapid_burst.sh    # conservative starting point, recommended for a first run
./run_rapid_burst.sh test1                     # append a name to the output directory
```

#### Configuration parameters

| Variable | Default | Description |
|----------|---------|-------------|
| `BURST_SIZE` | `1000` | SandboxClaims created per burst iteration |
| `QPS` | `1000` | Max creation rate (queries per second) |
| `TOTAL_BURSTS` | `10` | Total number of burst iterations |
| `WARMPOOL_SIZE` | `1000` | Pre-warmed sandboxes to maintain |
| `RUNTIME_CLASS` | `""` (none) | RuntimeClassName for the SandboxTemplate — set to `gvisor` if your cluster supports it |

> **Note:** These are the script's own defaults, sized for a well-tuned cluster. For a first run, or before raising controller concurrency, start with the more conservative `BURST_SIZE=400 QPS=400` shown above.

Total claims created = `BURST_SIZE × TOTAL_BURSTS`.

For maximum throughput testing, consider raising controller concurrency alongside `QPS`:

```yaml
args:
- --kube-api-qps=1000
- --kube-api-burst=2000
- --sandbox-concurrent-workers=400
- --sandbox-claim-concurrent-workers=400
- --sandbox-warm-pool-concurrent-workers=10
- --sandbox-template-concurrent-workers=2
```

#### Output

All artifacts (CL2 log, test overrides, Prometheus reports) are saved to a timestamped directory at `${TEST_DIR}/tmp/${RUN_ID}`.

---

## Metrics Collected

Recipes differ in which measurements they run and how they collect them. Some query Prometheus directly (CL2's `GenericPrometheusQuery` method); others use CL2's built-in `PodStartupLatency` or `SchedulingThroughput` methods, which don't go through Prometheus at all.

| Recipe | Measurements |
|---|---|
| `rapid-burst-test.yaml` | `SandboxStartupLatency` (`PodStartupLatency`, selector `app=agent-sandbox-load-test`), `SandboxClaimStartupLatency` (`GenericPrometheusQuery`, requires the timestamp-injection webhook below), `SandboxClaimControllerStartupLatency` (`GenericPrometheusQuery`) |
| `high-volume-test.yaml` | `SchedulingThroughput` (`SchedulingThroughput`), `SandboxStartupLatency` (`PodStartupLatency`, selector `group=linear-rampup`) |
| `medium-scale-concurrent-load-test.yaml` | `SandboxStartupLatency` (`PodStartupLatency`, selector `group=continuous-churn-group`) |
| `throughput-test.yaml` | `SchedulingThroughput` (`SchedulingThroughput`), `ReadyPerSecond` (`GenericPrometheusQuery`), `SandboxStartupLatency` (`PodStartupLatency`, selector `group=throughput-test`) |
| `warmpool-burst-test.yaml` | `ColdAcquisitionLatency` (`PodStartupLatency`, selector `latency-type=cold`) |

### SandboxClaim startup latency

Only `rapid-burst-test.yaml` collects this metric. It measures the end-to-end time from when the kube-apiserver receives the SandboxClaim create request to when the claim is marked as Ready (implying the claim, sandbox, and pod are all ready).

> **Note:** This metric requires a mutating admission webhook to record the start timestamp. See the [timestamp-injection webhook example](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/webhook-inject-timestamp/testing_webhook_guide.md) for setup instructions. Because the kube-apiserver and controller may run on different nodes, this metric may include clock skew.

| Metric | Prometheus query | Default threshold |
|--------|-----------------|-------------------|
| `StartupLatency50` | `histogram_quantile(0.50, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `StartupLatency90` | `histogram_quantile(0.90, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `StartupLatency99` | `histogram_quantile(0.99, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket{}[%v])) by (le))` | 5 000 ms |

### SandboxClaim controller startup latency

Only `rapid-burst-test.yaml` collects this metric. It measures the time from when the SandboxClaim controller's informer first observes the SandboxClaim creation to when the controller marks it as Ready. Because the informer sees the resource only after it has been persisted by the API server, this is a subset of the `agent_sandbox_claim_startup_latency_ms_bucket` metric — the delta between the two represents time spent in the kube-apiserver plus network propagation delay (plus or minus any clock skew). This metric has no clock skew of its own, since both the start and end timestamps are recorded by the same controller. Unlike `agent_sandbox_claim_startup_latency_ms_bucket`, this metric requires no webhook and works out of the box.

| Metric | Prometheus query | Default threshold |
|--------|-----------------|-------------------|
| `ControllerStartupLatency50` | `histogram_quantile(0.50, sum(rate(agent_sandbox_claim_controller_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `ControllerStartupLatency90` | `histogram_quantile(0.90, sum(rate(agent_sandbox_claim_controller_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `ControllerStartupLatency99` | `histogram_quantile(0.99, sum(rate(agent_sandbox_claim_controller_startup_latency_ms_bucket{}[%v])) by (le))` | 5 000 ms |

### Scheduling throughput and pod startup latency

These are two distinct measurements, backed by different CL2 methods:

- `SchedulingThroughput` (`high-volume-test.yaml`, `throughput-test.yaml`) — pod scheduling rate, via CL2's built-in `SchedulingThroughput` method.
- `SandboxStartupLatency` / `ColdAcquisitionLatency` (all five recipes) — pod startup latency, via CL2's built-in `PodStartupLatency` method. The label selector varies per recipe — see the table above. `rapid-burst-test.yaml` additionally collects the two Prometheus-backed SandboxClaim metrics documented below.

`throughput-test.yaml` also runs `ReadyPerSecond`, a `GenericPrometheusQuery` measurement; it isn't documented elsewhere in this guide.

The controller exposes all metrics at its `/metrics` endpoint; a Prometheus `ServiceMonitor` is provided at `dev/load-test/test-recipes/monitor/agent-sandbox-controller-monitor.yaml`.

---

## See Also

- [Controller Metrics Reference]({{< ref "/docs/metrics" >}}) — every metric family the controller exposes, with types, labels, and availability conditions
- [Configuration reference](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/configuration.md) — full flag reference for the controller
- [Running tests](../contribution-guidelines/testing/) — unit, integration and e2e test commands
- [ClusterLoader2 getting started](https://github.com/kubernetes/perf-tests/blob/master/clusterloader2/docs/GETTING_STARTED.md)
