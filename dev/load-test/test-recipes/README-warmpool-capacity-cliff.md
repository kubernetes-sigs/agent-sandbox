# WarmPool capacity cliff test

This ClusterLoader2 recipe finds the first load step where SandboxWarmPools fail
to converge within a configured deadline. It complements the
[Sandbox-only capacity test](README-capacity-cliff.md) and covers two scenarios:

- `single-pool`: one pool, with desired replicas increasing by `STEP_SIZE` each step.
- `many-pools`: pool count increases by `STEP_SIZE`, each pool has `POOL_SIZE`
  replicas, and all pools reference one template in one namespace. After capacity
  converges, change the shared template's `BENCHMARK_REVISION` container environment
  variable and wait for every pool to replace its members using `Recreate`. This
  exercises template watch fan-out.

## Prerequisites

Use a dedicated test cluster with the controller's extensions enabled. This test
creates and replaces Pods; its default scale is unsuitable for a shared cluster.
Supply a single kubeconfig file explicitly through `KUBECONFIG`. The runner passes
it to both CL2 and the `kubectl` convergence helper.

Install Go, Python 3, `kubectl`, and `jq`, and clone
[kubernetes/perf-tests](https://github.com/kubernetes/perf-tests). Set `CL2_DIR` to
its `clusterloader2` directory. The runner uses CL2's built-in `Exec` measurement;
no modifications to perf-tests are needed.

For large control-plane tests, follow the
[KWOK setup](README-capacity-cliff.md#node-capacity-kwok-vs-real-nodes) and use
`KWOK_NODES=true`. Real-node runs also measure node capacity, scheduling and image
pulls, so a timeout alone does not prove a controller bottleneck. Record controller
flags, resource limits, node capacity, image and CL2/controller revisions with results.

## Small local verification

Create a dedicated kind cluster (this command recreates the named cluster):

```bash
EXTENSIONS=true KIND_CLUSTER_NAME=warmpool-capacity make deploy-kind
export KUBECONFIG="$PWD/bin/KUBECONFIG"
export CL2_DIR="$HOME/perf-tests/clusterloader2"

# 1 -> 2 replicas in one pool.
WORKLOAD=warmpool WARMPOOL_SCENARIO=single-pool \
  PROVIDER=kind ENABLE_PROMETHEUS=false \
  STEP_SIZE=1 TOTAL_STEPS=2 HOLD_DURATION=1s \
  WARMPOOL_TIMEOUT_SECONDS=180 WARMPOOL_POLL_SECONDS=2 \
  bash dev/load-test/test-recipes/run_capacity_cliff.sh single-smoke

# 1 -> 2 pools, one member each; replace all members at each step.
WORKLOAD=warmpool WARMPOOL_SCENARIO=many-pools POOL_SIZE=1 \
  PROVIDER=kind ENABLE_PROMETHEUS=false \
  STEP_SIZE=1 TOTAL_STEPS=2 HOLD_DURATION=1s \
  WARMPOOL_TIMEOUT_SECONDS=180 WARMPOOL_POLL_SECONDS=2 \
  bash dev/load-test/test-recipes/run_capacity_cliff.sh fanout-smoke
```

These verify the workflow, not a capacity limit. `ENABLE_PROMETHEUS=false` skips
CL2's Prometheus installation/queries for a small smoke run; raw controller metrics
and resource snapshots are still attempted. Use the default `true` for load runs.
The existing Prometheus sizing and kind flags in `run_capacity_cliff.sh` apply.

## Load configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `WORKLOAD` | `sandbox` | Set to `warmpool` to select this recipe. |
| `WARMPOOL_SCENARIO` | `single-pool` | `single-pool` or `many-pools`. |
| `STEP_SIZE` | `1000` | Replicas added to one pool, or number of pools added. |
| `TOTAL_STEPS` | `20` | Number of load plateaus. |
| `POOL_SIZE` | `1` | Replicas per pool in `many-pools`. |
| `QPS` | `100` | CL2 object creation/update rate; not the controller's Pod creation rate. |
| `WARMPOOL_TIMEOUT_SECONDS` | `1800` | Deadline for each capacity/rollout wait. |
| `WARMPOOL_POLL_SECONDS` | `10` | Interval between status LISTs. |
| `HOLD_DURATION` | `2m` | Hold after convergence, before Prometheus gathering. |
| `KWOK_NODES` | `false` | Pin workload Pods to KWOK fake nodes. |
| `SANDBOX_IMAGE` | `registry.k8s.io/pause:3.10` | Must be pullable on the test nodes. |
| `ENABLE_PROMETHEUS` | `true` | Enable CL2 Prometheus and per-step queries. |
| `CONTROLLER_METRICS_PATH` | See below | API proxy path for raw metrics snapshots. |

`NAMESPACES` must remain `1`: the many-pool scenario specifically measures one
shared template's fan-out. Maximum Sandbox count is `STEP_SIZE * TOTAL_STEPS` for
`single-pool`, or `STEP_SIZE * TOTAL_STEPS * POOL_SIZE` for `many-pools`.
`CONVERGENCE_TIMEOUT` and `NETWORK_POLICY` belong to the Sandbox-only recipe;
WarmPools use the seconds setting above and the controller's template security defaults.

## Interpreting results

Artifacts are written under `dev/load-test/test-recipes/tmp/<run-id>/`. Each
`step-N-capacity/` and `step-N-rollout/` directory contains:

- `result.json`: target, revision, wait elapsed time, `converged`, `timeout`, or
  `error`, and explanations for objects that did not converge.
- `pools.json`, `sandboxes.json`, and namespace Pod/event snapshots.
- `controller-metrics.txt`: raw metrics including process memory, CPU counters,
  workqueue depth and reconcile histograms. The default proxy path is
  `/api/v1/namespaces/agent-sandbox-system/services/agent-sandbox-controller:metrics/proxy/metrics`.
  Override `CONTROLLER_METRICS_PATH` for a different Service/namespace. Inaccessible
  metrics or optional snapshots produce `.error` files, not a convergence failure.

Successful convergence requires current-generation pool counters with
`readyReplicas == replicas == spec.replicas`, the exact desired number of owned
Sandboxes, and every member Ready at its current generation with the expected
pod-template revision. Deleting members also prevent convergence. Pool generation
alone is insufficient: a shared template edit does not increment it, and old
members can still be Ready before the controller notices the change.

The first `timeout` is the failed load step; the preceding successful target is
only a tested lower bound under these settings. A rollout timeout is distinct from
an initial capacity timeout. `error` indicates an API/tooling failure and must not
be interpreted as a capacity cliff. The helper saves snapshots before returning
a failure. CL2 records the failure but continues its pre-expanded steps, then
exits nonzero: use the **first** failed
step when interpreting the cliff, even if later steps converge. Size the maximum
load accordingly. Raw snapshots remain available independently of Prometheus
gathering. CL2 normally deletes its test namespace afterward, so use the saved
snapshots for diagnosis.

Short or idle steps may omit rate-based metrics such as reconcile p99 when there
are insufficient samples or no reconciles between scrapes. Missing values are not
zero latency; the raw histogram counters remain in the per-stage snapshots.

A slow convergence can reflect insufficient timeout, deliberate controller refill
limits, unschedulable Pods, or image pulls. Inspect snapshots and metrics before
attributing the result to the controller. Polling and snapshot LISTs add load;
keep polling slow during large tests. The helper's elapsed time measures the wait
following the CL2 mutation phase, not end-to-end object submission latency.

Run convergence regression checks with:

```bash
python3 -m unittest discover -s dev/tools -p wait_for_warmpool_test.py
```

These are also collected by `make test-unit` in the existing dev-tools suite.
