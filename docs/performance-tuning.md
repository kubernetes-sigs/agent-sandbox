# Performance Tuning

This guide covers every controller knob that affects throughput and latency
under high claim rates, explains what each one does, and recommends concrete
values for two traffic profiles: **burst** (many claims at once, then quiet)
and **sustained** (continuous high arrival rate).

All flags are on the `agent-sandbox-controller` binary. Defaults are safe for
small deployments; the high-performance settings below are opt-in.

---

## Knobs at a glance

| Category | Flag | Default | Affects |
|---|---|---|---|
| API connections | `--api-connections` | 1 | Write throughput ceiling |
| API connections | `--separate-watch-connection` | false | Watch-stream latency under write bursts |
| Client throttling | `--kube-api-qps` | -1 (none) | Client-side request rate |
| Client throttling | `--kube-api-burst` | 10 | Client-side burst allowance |
| Concurrency | `--sandbox-concurrent-workers` | 100 | Sandbox reconcile parallelism |
| Concurrency | `--sandbox-claim-concurrent-workers` | 50 | SandboxClaim reconcile parallelism |
| Concurrency | `--sandbox-warm-pool-concurrent-workers` | 1 | SandboxWarmPool reconcile parallelism |
| Concurrency | `--sandbox-template-concurrent-workers` | 1 | SandboxTemplate reconcile parallelism |
| Warm pool | `--sandbox-warm-pool-max-batch-size` | 300 | Max sandboxes created per reconcile |
| Warm pool | `--sandbox-warm-pool-replenish-delay` | 0 | Delay before refill starts (burst insulation) |
| Warm pool | `--sandbox-warm-pool-max-refill-rate` | 0 (unpaced) | Refill rate cap, sandboxes/s per pool |
| Cache | `--cache-label-selectors` | false | Informer scope (O(cluster) → O(sandboxes)) |
| Write reduction | `--disable-claim-events` | false | Eliminate Kubernetes Event writes |
| Write reduction | `--disable-claim-observability-annotations` | false | Eliminate one annotation write per claim |

---

## API connection tuning

The kube-apiserver caps the number of concurrent in-flight HTTP/2 streams
per connection (typically 100, configurable via
`--http2-max-streams-per-connection`). A single HTTP/2 connection therefore
bounds the controller's effective write concurrency regardless of how many
workers are running.

### `--api-connections` (default: 1)

Opens N independent HTTP/2 connections for non-watch traffic (writes, status
patches, leader election). Requests are distributed round-robin across all
connections, multiplying the effective concurrency ceiling by N.

```
effective write concurrency ≈ N × per-connection stream limit
```

**Recommended:** `--api-connections=4` on clusters with ≥300 claims/s. Tune
upward if you see HTTP/2 GOAWAY or per-connection stream saturation in
controller logs.

### `--separate-watch-connection` (default: false)

Gives the informer cache (list/watch) a dedicated HTTP/2 connection. Without
this, large write bursts compete with watch frames on the same TCP connection,
delaying watch event delivery and slowing reconcile triggers.

**Recommended:** enable whenever `--api-connections` > 1, or whenever you
observe watch event latency spikes during write bursts.

### `--kube-api-qps` and `--kube-api-burst`

Client-side throttle applied before requests reach the API server. The
default QPS of -1 means no client-side throttle — the server's APF policy
controls admission instead, which is the preferred approach (see [APF
insulation](#api-priority-and-fairness-apf-insulation) below).

If you must apply client-side throttling, set `--kube-api-burst` ≥ the total
number of concurrent workers (`sandbox + claim + warm-pool + template`). A
burst limit lower than worker count causes client-side throttling before
requests reach the server, which negates the parallelism benefit.

---

## API Priority and Fairness (APF) insulation

The most important server-side tuning is applying the APF insulation overlay
at `examples/apf-insulation/apf-insulation.yaml`. This gives the controller
dedicated APF concurrency seats and splits its traffic into three priority
classes so refill bursts cannot queue out claim-adoption writes.

See [docs/apf-insulation.md](apf-insulation.md) for the full explanation,
seat-sizing math, and when to apply it.

**Recommended:** apply on any cluster where claim rates exceed ~50/s or where
other workloads share the API server.

---

## Concurrency workers

Workers control how many reconciles run in parallel within each controller.
Raising workers increases throughput but also increases API server load.

| Flag | When to raise | Practical ceiling |
|---|---|---|
| `--sandbox-concurrent-workers` | Many sandboxes in flux simultaneously | ~500; bounded by node scheduling capacity |
| `--sandbox-claim-concurrent-workers` | High claim arrival rate | ~200; bounded by API server write throughput |
| `--sandbox-warm-pool-concurrent-workers` | Many pools with independent refill | rarely > 4; pools are coarse-grained |
| `--sandbox-template-concurrent-workers` | Many templates changing frequently | rarely > 2 |

**Warning:** `sandbox + claim + warm-pool + template` workers total > 1000
triggers a startup warning. The practical ceiling is usually the API server's
mutating inflight limit, not the worker count.

---

## Warm pool refill shaping

When a burst of SandboxClaims adopts warm sandboxes, the warm-pool controller
immediately tries to refill the deficit. Without shaping, this refill burst
competes for API server write capacity with the very claim-adoption writes it
is meant to support. Two independent flags shape the refill:

### `--sandbox-warm-pool-max-batch-size` (default: 300)

Maximum sandboxes created per reconcile round. Large pools fill in
`ceil(replicas / batchSize)` round-trips. The default of 300 is safe at any
value under the expectations gate; raising it reduces fill time but increases
burst write pressure.

### `--sandbox-warm-pool-replenish-delay` (default: 0 — immediate)

Defers the **start** of refill after pool members are adopted. While claims
are still arriving and adopting sandboxes, the refill timer re-arms; it only
fires once the pool has been stable (no more drops) for the delay duration.
Initial pool fill and scale-ups are never delayed.

Use this for **burst traffic**: the burst is served from the warm pool at
warm latency, and refill begins only after the burst is absorbed.

**Caveat:** under continuous arrivals the delay re-arms indefinitely and the
pool drains. Use `--sandbox-warm-pool-max-refill-rate` instead for sustained
load, or combine a short delay with a rate cap.

### `--sandbox-warm-pool-max-refill-rate` (default: 0 — unpaced)

Caps the refill rate in sandboxes/second per pool via a per-pool token
bucket. Turns full-deficit burst creates into a smooth stream. The controller
requeues exactly when the next token accrues, so pacing does not rely on
watch events.

Use this for **sustained traffic**: keep `replenish-delay=0` and set the
rate ≥ your steady claim arrival rate so the pool never drains.

**Sizing guidance** from benchmarks (300 claims warm-adoption on k8s 1.35,
e2-standard-16 control plane, kops):

- Isolated per-pool refill ceiling is ~70–85 sandboxes/s (pod scheduling
  bounds at ~70/s at the kube-scheduler default `--kube-api-qps=50`).
- For N parallel pools at rate R each, you need
  `pools ≥ ceil(arrival_rate / R)` to keep up with steady-state demand.
- Under a 45/s Poisson arrival rate across 4 pools with `max-refill-rate=100`:
  p50 latency held at **92 ms** and p90 at **182 ms** across a full 60-second
  window with no pool drain. The same load with `replenish-delay=20s` and no
  rate cap served the first ~10 s at warm latency then settled to a ~1.4 s
  p50 cold steady state once the pool drained.

The two flags compose: `replenish-delay` defers the start of refill;
`max-refill-rate` shapes its flow once started.

---

## Informer cache scoping

### `--cache-label-selectors` (default: false)

Scopes the Pod and Service informer caches to objects carrying the sandbox
tracking label. The controller only ever creates Pods and Services it labeled
itself, so on large or shared clusters this cuts informer list/watch volume,
JSON decode CPU, and cache memory from O(cluster pods) to O(sandbox pods).

**Recommended:** enable on clusters with > ~5000 total pods or where the
controller's pod cache is a meaningful fraction of memory.

**Caveat:** externally pre-provisioned resources on the sandbox adoptable
path must also carry the tracking label to remain visible to the controller.

---

## Write reduction

During a claim burst, every avoidable write competes for API server capacity
with latency-critical adoption writes. Two flags eliminate non-essential
writes entirely:

### `--disable-claim-events` (default: false)

Disables Kubernetes Event emission from the SandboxClaim controller. Events
are informational only; removing them eliminates roughly one write per claim
lifecycle transition.

**Recommended:** enable during large claim bursts (> ~200 claims/s) or when
the events API level in APF is saturated.

### `--disable-claim-observability-annotations` (default: false)

Skips persisting the SandboxClaim observability annotations (controller
first-observed timestamp, trace context) to the API server. The values are
still computed in memory, so startup-latency metrics and trace propagation
within the current process keep working. The cost is losing the on-object
breadcrumb after a controller restart.

**Recommended:** enable when optimizing for lowest possible claim latency and
the annotation debugging breadcrumbs are not needed.

---

## Recommended configurations

### Burst profile

Traffic pattern: many claims arrive together, then the cluster is quiet.
Priority: serve the burst at warm latency; refill can start after.

```yaml
args:
  - --extensions
  # API server connections
  - --api-connections=4
  - --separate-watch-connection=true
  # Concurrency
  - --sandbox-concurrent-workers=200
  - --sandbox-claim-concurrent-workers=150
  - --sandbox-warm-pool-concurrent-workers=2
  # Warm pool: defer refill until the burst is absorbed
  - --sandbox-warm-pool-max-batch-size=300
  - --sandbox-warm-pool-replenish-delay=20s
  # Cache and write reduction
  - --cache-label-selectors=true
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

### Sustained high-throughput profile

Traffic pattern: continuous high arrival rate (e.g. 30–100 claims/s steady).
Priority: keep the pool topped up at the arrival rate; warm latency must hold
across the entire window.

```yaml
args:
  - --extensions
  # API server connections
  - --api-connections=4
  - --separate-watch-connection=true
  # Concurrency
  - --sandbox-concurrent-workers=200
  - --sandbox-claim-concurrent-workers=150
  - --sandbox-warm-pool-concurrent-workers=2
  # Warm pool: no delay; rate cap ≥ steady claim rate
  # (set per-pool rate ≥ arrival_rate / number_of_pools)
  - --sandbox-warm-pool-max-batch-size=300
  - --sandbox-warm-pool-replenish-delay=0
  - --sandbox-warm-pool-max-refill-rate=100
  # Cache and write reduction
  - --cache-label-selectors=true
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

**Sizing `--sandbox-warm-pool-max-refill-rate`:** set it ≥ your per-pool
steady claim arrival rate. If you have 4 pools and 80 claims/s total, 20/s
per pool is sufficient; 100/s leaves headroom for spikes. The per-pool
ceiling is ~70–85/s on a standard control plane, so values above 85 are only
useful if you have multiple pools and want burst headroom.

---

## Applying settings

**Direct manifest edit** (`extensions.yaml`):

```yaml
containers:
- name: agent-sandbox-controller
  args:
  - --extensions
  - --api-connections=4
  - --separate-watch-connection=true
  - --sandbox-warm-pool-replenish-delay=0
  - --sandbox-warm-pool-max-refill-rate=100
  - --cache-label-selectors=true
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
```

**Live cluster patch:**

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type='json' \
  -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--api-connections=4"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--separate-watch-connection=true"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-max-refill-rate=100"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--cache-label-selectors=true"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--disable-claim-events=true"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--disable-claim-observability-annotations=true"}
  ]'
```
