# Agent Sandbox Capacity Cliff Test (Test Recipes)

## Overview

This recipe answers one question: **how many Sandbox objects (each with a backing Pod) can a
cluster hold before performance falls off a cliff?**

It runs against a Kubernetes cluster using [ClusterLoader2](https://github.com/kubernetes/perf-tests)
(CL2) and, unlike the other recipes in this directory, **latency is not the primary measurement**.
The test ratchets the total Sandbox count upward in discrete steps and, at each step, waits for
every Sandbox to reach `Ready=True`. The pass/fail criterion is *convergence*: slow is acceptable,
failing to converge is not. The **cliff is the first step whose wait times out** — the previous
step's count is the cluster's upper bound.

At each plateau the test snapshots capacity metrics (controller memory, controller restarts,
workqueue depth, stored object counts, etcd DB size) so you can reconstruct the degradation curve
and identify which component gave out first. Pod startup latency is recorded across the whole run
as nice-to-know data (it shows where degradation *starts*, before the hard cliff) but never gates
the test.

Scope is deliberately narrow: only `Sandbox` and its child `Pod`. The Sandbox template disables the
headless Service (`service: false`) and uses no `volumeClaimTemplates`, and the extensions
(SandboxClaim / SandboxTemplate / SandboxWarmPool) are not exercised — run the controller **without**
`--extensions`.

## Prerequisites

- **Go Environment**: A working Go installation is required to compile and run ClusterLoader2.
- **Kubernetes Cluster**: `kubectl` access configured for the target cluster (config at
  `$HOME/.kube/config`).
- **Source Code Repositories**: Cloned to your local machine, typically in `$HOME`:
  - `perf-tests`: The official Kubernetes performance testing repository containing ClusterLoader2.
  - `agent-sandbox`: The main project repository.
- **Command-line Tools**: `jq` (used by the runner to build the CL2 overrides file and by the kwok
  install snippet below, which also uses `curl`).
- **`agent-sandbox-controller`**: Installed in the target cluster **without** the `--extensions`
  flag. See [README-rapid-burst.md](README-rapid-burst.md) for how to build/push a local image and
  generate manifests. Recommended controller args for this test:

  ```yaml
  containers:
    - name: agent-sandbox-controller
      args:
        - --leader-elect=true
        - --kube-api-qps=-1
        - --sandbox-concurrent-workers=1000
      resources:
        limits:
          # Set a fixed, production-representative memory limit. The controller's
          # informer caches grow with the number of Sandboxes and Pods, and an
          # OOMKill (visible as ControllerRestartsTotal climbing in the per-step
          # metrics) is one of the cleanest "capacity reached" signals this test
          # produces. An unbounded controller hides the cliff.
          memory: 2Gi
  ```

  Raising the workers and QPS matters even though this is not a throughput test: with the defaults
  (`--sandbox-concurrent-workers=1` and a low API burst) you would measure the controller's rate
  limiter, not the cluster's capacity.

### Node capacity: kwok vs. real nodes

Every Sandbox in this test gets a real Pod, so on real nodes the result is usually just
`nodes × max-pods-per-node` (110 by default) — a scheduling limit, not the control-plane limit you
are probably after. To find the **control-plane ceiling** (etcd, apiserver, controller), back the
test with [kwok](https://kwok.sigs.k8s.io/) fake nodes: pods scheduled to them are marked Running
and Ready without a kubelet, so the full Sandbox reconcile path is exercised at a fraction of the
cost.

Install kwok into the test cluster and create fake nodes (see the
[kwok docs](https://kwok.sigs.k8s.io/docs/user/kwok-in-cluster/) for the current release):

```bash
KWOK_REPO=kubernetes-sigs/kwok
KWOK_LATEST_RELEASE=$(curl -s "https://api.github.com/repos/${KWOK_REPO}/releases/latest" | jq -r '.tag_name')
kubectl apply -f "https://github.com/${KWOK_REPO}/releases/download/${KWOK_LATEST_RELEASE}/kwok.yaml"
kubectl apply -f "https://github.com/${KWOK_REPO}/releases/download/${KWOK_LATEST_RELEASE}/stage-fast.yaml"
```

```bash
# Create enough fake nodes for the target count: e.g. 200 nodes x 256 pods = 51,200 pods.
for i in $(seq 0 199); do
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Node
metadata:
  name: kwok-node-${i}
  labels:
    type: kwok
    kubernetes.io/role: agent
  annotations:
    kwok.x-k8s.io/node: fake
spec:
  taints:
  - key: kwok.x-k8s.io/node
    value: fake
    effect: NoSchedule
status:
  allocatable:
    cpu: "32"
    memory: 256Gi
    pods: "256"
  capacity:
    cpu: "32"
    memory: 256Gi
    pods: "256"
EOF
done
```

Then run the test with `KWOK_NODES=true`, which adds a `type: kwok` nodeSelector and the kwok taint
toleration to every sandbox pod. The taint keeps everything else — including the agent-sandbox
controller and the CL2-managed Prometheus — off the fake nodes.

## Running the Test

```bash
cd dev/load-test/test-recipes
chmod +x run_capacity_cliff.sh   # first time only
./run_capacity_cliff.sh
```

You can optionally pass a name which will be appended to the output directory for the test
artifacts:

```bash
KWOK_NODES=true STEP_SIZE=2000 TOTAL_STEPS=25 ./run_capacity_cliff.sh kwok-50k
```

Use a coarse first pass to bracket the cliff, then re-run with a smaller `STEP_SIZE` around the
failure point for a tighter bound. CL2 expands the step loop up front and cannot bisect at runtime,
so the cliff resolution equals the step size.

## Configuration

Overridable via environment variables on `run_capacity_cliff.sh`:

- **`STEP_SIZE`**: Sandboxes added per namespace at each step (default: `1000`).
- **`TOTAL_STEPS`**: Number of ramp steps (default: `20`). The maximum attempted count is
  `STEP_SIZE * TOTAL_STEPS * NAMESPACES`. **Deliberately overshoot the expected ceiling** — the
  step that fails to converge is the result you are looking for.
- **`NAMESPACES`**: Namespaces to spread the load across (default: `1`).
- **`QPS`**: Object creation rate (default: `100`).
- **`HOLD_DURATION`**: How long to hold each plateau before snapshotting metrics (default: `2m`).
  The hold exposes steady-state load (resyncs, watch churn) that a pure creation burst hides.
- **`CONVERGENCE_TIMEOUT`**: Per-step timeout for all Sandboxes to reach `Ready=True`
  (default: `30m`). This is the operative definition of "does not fit". Size it generously —
  roughly `STEP_SIZE / conservative-sandboxes-per-second` with a 3–5x margin — so that a timeout
  means "cannot converge", never "converged slowly".
- **`KWOK_NODES`**: Set to `true` to pin sandbox pods to kwok fake nodes (default: `false`).
- **`PROVIDER`**: CL2 provider, e.g. `gke` or `kind` (default: `gke`).
- **`SANDBOX_IMAGE`**: Sandbox pod container image (default: `registry.k8s.io/pause:3.10`). Must
  be pullable from the cluster's nodes — private GKE nodes without Cloud NAT cannot reach
  `registry.k8s.io`, so point this at a `gcr.io`/`pkg.dev`-hosted image there (e.g.
  `gcr.io/google-containers/pause:3.2`).
- **`NETWORK_POLICY`**: Set to `true` to apply one NetworkPolicy per test namespace selecting all
  sandbox pods (default: `false`). The policy mirrors the SandboxTemplate extension's "Secure by
  Default" spec (ingress from the sandbox-router only, egress to public internet with private and
  link-local CIDRs blocked) with the test's shared `group` label standing in for the shared
  template-ref-hash label. Use this to measure the production configuration — policy enforcement is
  dataplane work, and an unpoliced run is the dataplane's best case. When comparing runs, watch the
  `StoredCiliumIdentities` column (Cilium dataplanes only): ~flat means the policy is compatible
  with shared identities; growing per-sandbox means identity cardinality is exploding through the
  policy path, which is the dataplane cliff signature.

### Preventing Prometheus OOM

CL2 sets the Prometheus container's memory request **and hard limit** to
`MEMORY_LIMIT_FACTOR Gi × (1 + nodes/1000)`. The default factor of 2 means a 2 Gi cap on any
cluster under 1,000 nodes — far too small for the pod counts this test creates, and the usual
reason CL2's Prometheus OOMs mid-run. Knobs (all runner env vars):

- **`PROMETHEUS_MEMORY_LIMIT_FACTOR`** (default `2`): the multiplier above. Rule of thumb for this
  test: ~2 Gi base + ~1 Gi per 10k pods at target scale, rounded up generously — the limit is a
  hard cap, so err high if the node can afford it.
- **`PROMETHEUS_NODE_SELECTOR`** (default empty): YAML fragment merged into Prometheus's
  nodeSelector, e.g. `"cloud.google.com/gke-nodepool: big-node-pool"`. Pin Prometheus to a node
  large enough for the factor you chose (remember: request = limit, so the full amount must be
  allocatable).
- **`SCRAPE_KUBE_PROXY`** (default `false` — deviates from CL2's default of `true`): kube-proxy is
  one scrape target *per node* and contributes nothing to this test; on large clusters it is pure
  Prometheus memory burn.
- **`PROMETHEUS_SLOW_APISERVER`** (default `false`): set `true` on large clusters to drop the
  apiserver scrape interval from 5s to 30s — a large reduction in ingestion load at the cost of
  coarser (still ample, given multi-minute plateaus) resolution for the per-step snapshots.

Also relevant: kubelet/cAdvisor scraping stays off by default (CL2's default), which is by far the
largest cardinality source on big clusters — leave it off.

## Interpreting the Results

All artifacts land in a timestamped directory at `${TEST_DIR}/tmp/${RUN_ID}` (full CL2 log,
generated overrides, per-measurement JSON, Prometheus reports).

1. **Find the cliff step.** Search the CL2 log / `junit.xml` for the first failed
   `Wait for ... Sandboxes Ready` step. The preceding step's sandbox count is the upper bound. If
   every step passed, the cluster fits the full `STEP_SIZE * TOTAL_STEPS * NAMESPACES` — raise
   `TOTAL_STEPS` and go again.
2. **Identify what gave out.** Each step produces a
   `GenericPrometheusQuery Sandbox Capacity Step N` JSON file. Tabulating them against the sandbox
   count shows which resource hit its wall at the cliff:
   - `ControllerResidentMemoryBytes` — informer caches hold every Sandbox and Pod (both carry a
     full PodSpec), so this should grow roughly linearly. A flatline near the container limit
     followed by restarts means the controller OOMed.
   - `ControllerUptimeSeconds` — a value that *drops* between steps means the controller restarted
     (e.g. OOMKilled); that is a cliff signal on its own, even if the step eventually converged.
   - `SandboxWorkqueueDepthMax` / `SandboxWorkqueueSecondsP99` — a queue that stops draining is the
     leading indicator, usually visible a step or two before convergence fails.
   - `EtcdDBSizeBytes` and `StoredSandboxObjects` — the per-step slope gives bytes-per-sandbox;
     extrapolate against the etcd quota (commonly 2–8 GiB) to predict the etcd-bound ceiling.
     `StoredSandboxObjects`/`StoredPodObjects` need a scrapable apiserver: the runner configures
     this automatically for kind (apiserver-only scraping on port 6443); managed control planes
     such as GKE expose nothing. `EtcdDBSizeBytes` additionally needs scrapable etcd, which only
     kOps/self-managed clusters provide — kind's etcd binds its metrics port to localhost, so
     expect this column to be empty on kind.
3. **Read the degradation curve (nice-to-know).** Per-step wall-clock durations in `junit.xml`
   show convergence time growing with total count — where it bends upward is where degradation
   begins, typically before the hard cliff. The `PodStartupLatency` summary covers the whole run;
   its threshold is set to 1h so it reports data without ever failing the test.

### Recommended follow-up at the max plateau

The informer re-list on startup is often the first thing to break at scale, and it is the failure
you would hit during a controller upgrade. After a successful run (or at the last converged
plateau), restart the controller and time how long it takes to become Ready and resume
reconciling — CL2 cannot express this, so do it manually before the sandboxes are deleted:

```bash
kubectl -n agent-sandbox-system rollout restart deployment agent-sandbox-controller
kubectl -n agent-sandbox-system rollout status deployment agent-sandbox-controller
```

## Cleanup

The final test step scales the Sandbox count to zero and CL2 deletes its auto-managed namespaces on
exit. Two caveats at high object counts:

- Deleting tens of thousands of Sandboxes takes a while, and a wedged controller at the cliff makes
  it slower — finalizer-bearing objects only go away as fast as the controller can process them.
  Budget for teardown, and if the run aborted early, sweep manually:

  ```bash
  kubectl get namespaces -o name | grep sandbox-capacity
  kubectl delete namespace <each sandbox-capacity-* namespace>
  ```

- Fake nodes are not managed by the test:

  ```bash
  kubectl delete nodes -l type=kwok
  ```
