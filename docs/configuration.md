# Configuration

The `agent-sandbox-controller` supports several command-line flags to tune performance and scalability under high load or in large clusters.

## Concurrency Settings

* `--sandbox-concurrent-workers` (default: 100): The maximum number of concurrent reconciles for the Sandbox controller.
* `--sandbox-claim-concurrent-workers` (default: 50): The maximum number of concurrent reconciles for the SandboxClaim controller.
* `--sandbox-warm-pool-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxWarmPool controller. Reconciles for a given pool key are serialized by the workqueue, so workers provide concurrency across distinct warm pools rather than within a single pool. Size this to the number of active warm pools in the cluster and available API capacity (e.g. 1 is sufficient if managing a single warm pool).
* `--sandbox-template-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxTemplate controller.
* `--sandbox-warm-pool-max-batch-size` (default: 300): The maximum number of sandboxes the SandboxWarmPool controller will create/delete in a single batch. Creates advance one observed batch per watch round-trip (the expectations gate waits for a batch's add events before issuing the next), so a large pool fills in about `ceil(replicas/batchSize)` round-trips; raising this trades round-trips for burst size and is safe at any value under the gate.
* `--sandbox-warm-pool-readiness-grace-period` (default: `5m`): How long a warm pool sandbox may stay non-Ready before the SandboxWarmPool controller considers it stuck and replaces it (or holds it, if its pod is unschedulable). Raise this for images with long initialization or clusters with slow node auto-provisioning. Must be a positive duration.
* `--sandbox-warm-pool-unschedulable-recheck-interval` (default: `1m`): Requeue interval at which the SandboxWarmPool controller re-checks a pool holding unschedulable sandboxes past the readiness grace period. Must be a positive duration.
* `--kube-api-qps` (default: -1, no client-side rate limiting): Client-side QPS limit for the Kubernetes API client.
* `--kube-api-burst` (default: 10): The maximum burst for client-side throttling of the Kubernetes API client. Only applies when `--kube-api-qps` is set to a positive value (client-side rate limiting enabled). When running high worker concurrency with a positive `--kube-api-qps`, raise this (e.g. 100–200) to match or exceed worker concurrency to avoid client-side throttling.

## API Transport and Connection Settings

* `--separate-watch-connection` (default: `false`): Give the manager's informer cache (list/watch streams) a dedicated HTTP/2 connection to the API server. Watch events arrive on existing long-lived streams, so this isolates their frames from TCP/connection-level queuing behind bursts of request traffic on a shared connection (HTTP/2 stream prioritization does not help in practice). Strongly recommended for high-throughput or bursty environments to prevent watch starvation.
* `--api-connections` (default: `1`): Number of independent HTTP/2 connections to the API server for non-watch traffic (writes, uncached reads, events, leader election). The `kube-apiserver` caps concurrent in-flight requests per HTTP/2 connection (`SETTINGS_MAX_CONCURRENT_STREAMS`; 100 by default, configurable server-side via `--http2-max-streams-per-connection`), so a single connection bounds effective concurrency at the advertised limit regardless of worker count or QPS settings. Values > 1 shard requests round-robin across that many dedicated connections (~N x per-connection limit ceiling).

## Warm Pool Replenishment Shaping

* `--sandbox-warm-pool-max-refill-rate` (default: `0`, unpaced): Max rate (sandboxes/second, per pool) at which the SandboxWarmPool controller creates replacement sandboxes, pacing replenishment via a token bucket into a smooth stream instead of full-deficit bursts that flood the write path and compete with claim adoption. `0` (default) leaves refill unpaced (whole deficit per reconcile).
* `--sandbox-warm-pool-replenish-delay` (default: `0`): How long the SandboxWarmPool controller defers creating replacement sandboxes after pool members drop out of the pool (e.g. a burst of SandboxClaims adopting warm sandboxes), so the burst gets API server priority. The hold re-arms while members keep dropping. `0` (default) replenishes immediately.
* `--enable-warm-pool-eviction` (default: `true`): Mark pods created by a warm pool as ready-to-evict by default.

## API Write and Cache Optimization

* `--disable-claim-events` (default: `false`): Disable Kubernetes Event emission from the SandboxClaim controller (its `Eventf` calls become no-ops), reducing API server and etcd write volume during large claim bursts.
* `--disable-claim-observability-annotations` (default: `false`): Skip persisting the SandboxClaim observability annotations (controller first-observed timestamp, trace context), removing one API write per claim. The values are still stamped on the in-memory object, so startup-latency metrics and trace propagation keep working within the controller process.
* `--cache-label-selectors` (default: `false`): Scope the manager's Pod and Service informer caches to objects carrying the sandbox tracking label (`agents.x-k8s.io/sandbox-name-hash`). The controller only ever creates/looks up Pods and Services it labeled itself, so on shared or high-churn clusters this cuts informer list/watch volume, JSON decode CPU, and cache memory from O(cluster) to O(sandboxes). CAVEAT: externally pre-provisioned resources that rely on the `agents.x-k8s.io/adoptable=true` adoption path MUST also carry the tracking label (value = the owning sandbox's name hash) to remain visible to the controller when this flag is enabled.
* `--sandbox-write-behind-window` (default: `0`): Coalescing window for the Sandbox controller's recoverable metadata-only writes. `0` disables coalescing.

## Cluster Settings

* `--cluster-domain` (default: `cluster.local`): The Kubernetes cluster domain used to
  construct service FQDNs. Only change this if your cluster is configured with a non-default
  domain (e.g. `my-company.local`).

## Deployment Example

To deploy the controller with custom concurrency settings, modify the `args` of the `agent-sandbox-controller` container within the project's installation manifests. 

If using the core controller, update `sandbox.yaml`:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --sandbox-concurrent-workers=10
```

If you are deploying the extensions controller (which includes the core controllers + extensions), update the args in `extensions.yaml` instead:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --extensions
        - --sandbox-concurrent-workers=10
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-warm-pool-concurrent-workers=10
        - --sandbox-warm-pool-max-batch-size=500
```
**Using `kubectl patch` (Live Cluster):**
If you have already deployed the controller (e.g., via `make deploy-kind`) and want to apply these concurrency flags dynamically to the running cluster, you can use a JSON patch:

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type='json' \
  -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-claim-concurrent-workers=100"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-max-batch-size=500"}
  ]'
```
This method safely appends the new flags without overwriting existing necessary arguments like `--leader-elect=true` or `--extensions=true`.

**Using Kustomize:**
If you prefer applying patches via Kustomize rather than modifying the base manifests directly, you can create a patch file (e.g., `patch-args.yaml`):

```yaml
# patch-args.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-sandbox-controller
  namespace: agent-sandbox-system
spec:
  template:
    spec:
      containers:
      - name: agent-sandbox-controller
        args:
        - --sandbox-concurrent-workers=10
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-warm-pool-concurrent-workers=10
        - --sandbox-warm-pool-max-batch-size=500
```
Then include the patch in your `kustomization.yaml`:
```yaml
patches:
  - path: patch-args.yaml
```

## High-Throughput & Scale Tuning

When running high sustained claim rates (e.g., 10–20+ claims/second) or managing large warm pools (e.g., 1,000–2,500+ replicas), standard controller deployments can experience API server bottlenecks and warm-pool replenishment stalls:

1. **Watch Stream Starvation & The Expectations Gate**:
   The `SandboxWarmPool` reconciler gates sandbox creation using an in-flight expectations tracker (`warmPoolExpectations`). When creating replacement sandboxes, it waits for the informer cache to observe all watch `ADD` events before issuing further creates.
   If writes and watch streams share a single HTTP/2 connection (`--separate-watch-connection=false`), large write bursts queue or drop watch frames. This causes create expectations to remain unsatisfied, wedging the warm pool until the 5-minute fallback timeout (`expectationsTimeout = 5m`) expires.
   * **Fix**: Enable `--separate-watch-connection=true` and shard write traffic with `--api-connections=4` (or `8`).

2. **Replenishment Pacing**:
   By default, replenishment is unpaced (`--sandbox-warm-pool-max-refill-rate=0`), causing the controller to attempt to satisfy the entire deficit at once in giant batches. This floods the API server with parallel POST requests, triggering API Priority & Fairness (APF) throttling.
   * **Fix**: Set `--sandbox-warm-pool-max-refill-rate` (e.g. `25`–`30` for a sustained 20 claims/s workload) to pace replenishment smoothly, and bound `--sandbox-warm-pool-max-batch-size=200`.

3. **Reducing API Server & etcd Pressure**:
   At high throughput, routine event generation and annotation updates write heavily to etcd:
   * **Fix**: Enable `--disable-claim-events=true` and `--disable-claim-observability-annotations=true` to eliminate ~40 write QPS at 20 claims/s.
   * **Fix**: Enable `--cache-label-selectors=true` to avoid caching non-sandbox pods and services across the cluster.
   * **Fix**: If using client-side rate limiting (`--kube-api-qps`), ensure `--kube-api-burst` (e.g. `200`) is sized to match or exceed worker concurrency to prevent client-side throttling. By default, `--kube-api-qps=-1` (client-side rate limiting is disabled).

### High-Throughput Deployment Example

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-sandbox-controller
  namespace: agent-sandbox-system
  labels:
    app: agent-sandbox-controller
spec:
  replicas: 1
  selector:
    matchLabels:
      app: agent-sandbox-controller
  template:
    metadata:
      labels:
        app: agent-sandbox-controller
    spec:
      serviceAccountName: agent-sandbox-controller
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller # replace with published image or deploy with ko
        args:
        - --leader-elect=true
        - --extensions=true
        # Connection sharding and watch isolation
        - --separate-watch-connection=true
        - --api-connections=4
        # Replenishment pacing
        - --sandbox-warm-pool-max-refill-rate=25
        - --sandbox-warm-pool-max-batch-size=200
        # Write and cache optimization
        - --disable-claim-events=true
        - --disable-claim-observability-annotations=true
        - --cache-label-selectors=true
        # Worker concurrency (size warm-pool workers to the number of distinct pools)
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-concurrent-workers=150
        - --sandbox-warm-pool-concurrent-workers=1
```

## SandboxClaim label-domain allowlist

Since v0.5.0, label keys in `SandboxClaim.spec.additionalPodMetadata.labels`
must carry a domain prefix from an allowlist; claims with any other label
domain are rejected with `Ready=False, reason=InvalidMetadata`. The default
allowlist is `sandbox.users.io` (subdomains of an allowed domain also pass).

The extensions controller reads the allowlist **once at startup** from
`/etc/sandbox-config/allowed-label-domains`, mounted from an optional
ConfigMap named `agent-sandbox-config` in the controller's namespace. The
file's contents **replace** the default rather than extending it, so include
`sandbox.users.io` if existing consumers rely on it. Domains are separated
by newlines or commas. An empty (or separator-only) file does not disable
the allowlist — the controller treats it the same as an absent file and
falls back to the `sandbox.users.io` default.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-sandbox-config
  namespace: agent-sandbox-system
data:
  allowed-label-domains: |
    example.com
    sandbox.users.io
```

After creating or changing the ConfigMap, restart the controller so it
re-reads the file:

```sh
kubectl -n agent-sandbox-system rollout restart deploy/agent-sandbox-controller
```

Annotations in `additionalPodMetadata` are governed separately by a
restricted-domain blocklist (with `cluster-autoscaler.kubernetes.io/safe-to-evict`
exempted), not by this allowlist.
