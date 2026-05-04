---
title: "Performance Assessment"
linkTitle: "Performance Assessment"
weight: 3
description: >
  How to measure, benchmark, and tune the performance of the Agent Sandbox controller.
---

This guide covers the tools and techniques available for assessing the performance of Agent Sandbox — from tuning controller concurrency, to running load tests, to collecting and interpreting benchmark results.

## Controller Performance Tuning

The `agent-sandbox-controller` exposes several flags that directly affect throughput and API server pressure. Raising these is the first step before running any load test.

| Flag | Default | Description |
|------|---------|-------------|
| `--sandbox-concurrent-workers` | `1` | Max concurrent reconciles for the Sandbox controller |
| `--sandbox-claim-concurrent-workers` | `1` | Max concurrent reconciles for the SandboxClaim controller |
| `--sandbox-warm-pool-concurrent-workers` | `1` | Max concurrent reconciles for the SandboxWarmPool controller |
| `--sandbox-template-concurrent-workers` | `1` | Max concurrent reconciles for the SandboxTemplate controller |
| `--kube-api-qps` | `-1` (unlimited) | Max QPS sent to the Kubernetes API server |
| `--kube-api-burst` | `10` | Max burst for API server throttle requests |

### Applying Flags

**Via manifest** — edit the relevant manifest for your deployment:

*Core install (`manifest.yaml`):*

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

*Extensions install (`extensions.yaml`) — note the additional `--extensions` flag, which enables the SandboxTemplate controller and its associated RBAC:*

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

Reference the patch from `kustomization.yaml` using `patches` with an explicit `target` and `options.type: json`:

```yaml
# kustomization.yaml
patches:
  - path: patch-args.yaml
    target:
      kind: Deployment
      name: agent-sandbox-controller
      namespace: agent-sandbox-system
    options:
      type: json
```

---

## E2E Benchmarks

The repository ships Go benchmarks in `test/e2e/` that measure Sandbox and SandboxClaim startup latency against a live cluster.

### Available benchmarks

| Benchmark | File | What it measures |
|-----------|------|-----------------|
| `BenchmarkChromeSandboxStartup` | `chromesandbox_test.go` | Chrome Sandbox pod startup latency |
| `BenchmarkChromeSandboxClaimStartup` | `chromesandbox_claim_test.go` | Chrome SandboxClaim end-to-end startup latency |

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
</testsuite>
```

---

## Test Recipes

`dev/load-test/test-recipes/` contains ready-made scenarios for more demanding performance testing. Each recipe uses Prometheus to collect detailed metrics.

### Available recipes

| Recipe | File | Purpose |
|--------|------|---------|
| Rapid burst | `rapid-burst-test.yaml` | Creates SandboxClaims in discrete high-rate bursts |
| High volume ramp | `high-volume-test.yaml` | Ramps creation rate up then back down |
| Steady-state churn | `medium-scale-concurrent-load-test.yaml` | Measures sustained concurrent churn |
| Throughput | `throughput-test.yaml` | Measures raw creation throughput |
| Warm pool burst | `warmpool-burst-test.yaml` | Tests warm pool performance under burst load |

### Running the rapid burst test

The rapid burst test is the primary scalability scenario. It creates SandboxClaims in repeated bursts and records per-burst latency distributions.

```bash
cd dev/load-test/test-recipes
chmod +x run_rapid_burst.sh
./run_rapid_burst.sh          # default parameters
./run_rapid_burst.sh test1    # append a name to the output directory
```

#### Configuration parameters

| Variable | Default | Description |
|----------|---------|-------------|
| `BURST_SIZE` | `1000` | SandboxClaims created per burst iteration |
| `QPS` | `1000` | Max creation rate (queries per second) |
| `TOTAL_BURSTS` | `10` | Total number of burst iterations |
| `WARMPOOL_SIZE` | `1000` | Pre-warmed sandboxes to maintain |
| `RUNTIME_CLASS` | `""` (none) | RuntimeClassName for the SandboxTemplate — set to `gvisor` if your cluster supports it |

Total claims created = `BURST_SIZE × TOTAL_BURSTS`.

For maximum throughput testing, consider raising controller concurrency alongside `QPS`:

```yaml
args:
- --kube-api-qps=1000
- --kube-api-burst=1000
- --sandbox-concurrent-workers=1000
- --sandbox-claim-concurrent-workers=1000
- --sandbox-warm-pool-concurrent-workers=1000
- --sandbox-template-concurrent-workers=1000
```

#### Output

All artifacts (CL2 log, test overrides, Prometheus reports) are saved to a timestamped directory at `${TEST_DIR}/tmp/${RUN_ID}`.

---

## Metrics Collected

All load test recipes collect the following Prometheus-backed metrics:

### SandboxClaim startup latency

Measures the end-to-end time from SandboxClaim creation to the underlying pod being ready.

| Metric | Prometheus query | Default threshold |
|--------|-----------------|-------------------|
| `StartupLatency50` | `histogram_quantile(0.50, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `StartupLatency90` | `histogram_quantile(0.90, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `StartupLatency99` | `histogram_quantile(0.99, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket{}[%v])) by (le))` | 5 000 ms |

### SandboxClaim controller startup latency

Measures the time the controller spends processing each SandboxClaim reconcile loop.

| Metric | Prometheus query | Default threshold |
|--------|-----------------|-------------------|
| `ControllerStartupLatency50` | `histogram_quantile(0.50, sum(rate(agent_sandbox_claim_controller_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `ControllerStartupLatency90` | `histogram_quantile(0.90, sum(rate(agent_sandbox_claim_controller_startup_latency_ms_bucket{}[%v])) by (le))` | 1 000 ms |
| `ControllerStartupLatency99` | `histogram_quantile(0.99, sum(rate(agent_sandbox_claim_controller_startup_latency_ms_bucket{}[%v])) by (le))` | 5 000 ms |

### Scheduling throughput

`SchedulingThroughput` — measured via CL2's built-in `PodStartupLatency` method, tracking pods labelled `app=agent-sandbox-load-test`.

The controller exposes all metrics at its `/metrics` endpoint; a Prometheus `ServiceMonitor` is provided at `dev/load-test/test-recipes/monitor/agent-sandbox-controller-monitor.yaml`.

---

## See Also

- [Configuration reference](https://github.com/volatilemolotov/agent-sandbox/blob/main/docs/configuration.md) — full flag reference for the controller
- [Running tests](../../contribution-guidelines/testing/) — unit, integration and e2e test commands
- [ClusterLoader2 getting started](https://github.com/kubernetes/perf-tests/blob/master/clusterloader2/docs/GETTING_STARTED.md)
