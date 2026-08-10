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

## Benchmark data

These numbers come from two sources:

**PR-level benchmarks** (kops, k8s 1.35.6, e2-standard-16 control plane): the
authoritative warm-adoption numbers, run against a dedicated cluster under
controlled conditions.

**GKE live tests** (barkland-brust, k8s 1.36.2, 20× e2-standard-16 worker
nodes, 75-claim burst from a 150-sandbox warm pool): A/B comparison across
all six flag combinations on the same live cluster and same binary, measuring
relative impact. Pod cold-start on this cluster during the test was ~42–50 s
p50 due to node churn from a concurrent upgrade cycle; warm-adoption latency
(sub-second when the pool is healthy) is covered by the PR-level data.

### GKE live A/B results — relative impact

| Config | p50 | p90 | p99 | vs baseline |
|---|---|---|---|---|
| A: Baseline (workers 1000/1000/500/100, batch 500) | 50.4 s | 88.2 s | 96.8 s | — |
| B: + `--api-connections=4 --separate-watch-connection` | 51.2 s | 92.9 s | 102.0 s | neutral |
| C: + `--disable-claim-events --disable-claim-observability-annotations` | 51.1 s | 93.0 s | 101.7 s | neutral |
| D: + `--sandbox-warm-pool-replenish-delay=15s` | 58.0 s | **297.6 s** | 306.0 s | **+3.4× p90** ⚠️ |
| E: + `--sandbox-warm-pool-max-refill-rate=80` | **42.2 s** | **75.3 s** | 82.7 s | **−15% p50/p90** |
| F: All flags combined | **42.1 s** | **75.3 s** | 82.6 s | same as E |

**What this shows:**

- `--sandbox-warm-pool-max-refill-rate` consistently reduces latency (~15%)
  by smoothing the pod creation burst and reducing scheduler pressure. Safe
  to enable broadly.
- `--api-connections` and write-reduction flags have negligible impact on
  per-claim latency in isolation; their value is at higher scale where API
  server concurrency is the bottleneck (see below).
- `--sandbox-warm-pool-replenish-delay` caused catastrophic p90 tail latency
  (88 s → 298 s) when claims fell through to cold start due to pool
  undersizing or pod evictions. **Do not enable this flag without first
  confirming your pool never drains under load** (see the `replenish-delay`
  section below).

### PR-level benchmark — warm-adoption latency

Under a 45/s Poisson arrival rate across 4 pools (113 replicas/ns) on kops:

| Config | p50 | p90 | p99 |
|---|---|---|---|
| Burst-tuned (`replenish-delay=20s`) | ~57 ms (first ~10 s, then ~1.4 s cold) | 2.28 s steady-state | — |
| Sustained-tuned (`replenish-delay=0`, `max-refill-rate=100`) | **92 ms** | **182 ms** | 376 ms |

The sustained config held p50 flat at 77–116 ms across all six 10-second
windows with no pool drain. Per-pool refill ceiling is ~70–85 sandboxes/s
(pod scheduling bounds at ~70/s at the kube-scheduler default
`--kube-api-qps=50`).

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

Live testing showed no impact on per-claim latency at moderate scale (75
concurrent claims). The value appears at high scale (≥300 claims/s) where a
single connection's stream limit becomes the saturation point.

**Recommended:** `--api-connections=4` on clusters with ≥300 claims/s. Tune
upward if you observe HTTP/2 GOAWAY or stream saturation in controller logs.

### `--separate-watch-connection` (default: false)

Gives the informer cache (list/watch) a dedicated HTTP/2 connection. Without
this, large write bursts can delay watch event delivery on a shared connection.

**Recommended:** enable alongside `--api-connections` > 1 for consistency;
measure watch event latency before concluding it is worth enabling alone.

### `--kube-api-qps` and `--kube-api-burst`

Client-side throttle applied before requests reach the API server. The
default QPS of -1 means no client-side throttle — the server's APF policy
controls admission instead, which is the preferred approach (see [APF
insulation](#api-priority-and-fairness-apf-insulation) below).

If you must apply client-side throttling, set `--kube-api-burst` ≥ the total
number of concurrent workers (`sandbox + claim + warm-pool + template`). A
burst limit lower than worker count causes client-side throttling before
requests reach the server, negating the parallelism benefit.

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
value; raising it reduces fill time at the cost of burst write pressure.

### `--sandbox-warm-pool-max-refill-rate` (default: 0 — unpaced)

Caps the refill rate in sandboxes/second per pool via a per-pool token
bucket. Turns full-deficit burst creates into a smooth stream, reducing
scheduler contention. The controller requeues exactly when the next token
accrues, so pacing does not rely on watch events.

Live testing confirmed a consistent **~15% reduction in p50 and p90 latency**
at 80 sandboxes/s. This flag is safe to enable broadly — it helps even when
claims fall through to cold start (no warm sandboxes available).

**Sizing:** set per-pool rate ≥ your steady claim arrival rate to prevent pool
drain. If you have 4 pools and 80 claims/s total, 20/s per pool is sufficient;
100/s leaves headroom for spikes. The per-pool ceiling on a standard control
plane is ~70–85/s.

For sustained high-throughput: keep `replenish-delay=0` and set rate ≥ your
steady arrival rate per pool so the pool never drains.

### `--sandbox-warm-pool-replenish-delay` (default: 0 — immediate)

Defers the **start** of refill after pool members are adopted. While claims
are still arriving, the timer re-arms; it only fires once the pool is stable
for the delay duration. Initial pool fill and scale-ups are never delayed.

**⚠️ Risk:** Live testing showed that when claims fall through to cold start
for any reason (pool undersized, pod evictions, node pressure), this flag
causes catastrophic tail latency. In the GKE A/B test, `replenish-delay=15s`
raised p90 from 88 s to **298 s** — a 3.4× regression — because 5 of 75
claims arrived when the pool had drained and the delay prevented immediate
replenishment.

**When it is safe to use:** only enable this flag if you have confirmed that
your pool is consistently larger than your burst size and that warm adoption
is actually occurring (i.e. claim latency is in the sub-second range, not
tens of seconds). In that condition, `replenish-delay` correctly defers
refill so the burst gets API server priority first.

**Do not use this flag** as a "set and forget" optimization. Monitor claim
p90/p99 after enabling it; a sudden jump is the signature of pool drain.

The two flags compose: `replenish-delay` defers the start of refill;
`max-refill-rate` shapes its flow once started. In most cases `max-refill-rate`
alone is the safer choice.

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

Live testing showed no measurable per-claim latency improvement in isolation
at moderate scale; the benefit appears when the events API level in APF is
saturated or at claim rates above ~200/s.

**Recommended:** enable when tuning for maximum throughput at high scale or
when the events API level is a bottleneck.

### `--disable-claim-observability-annotations` (default: false)

Skips persisting the SandboxClaim observability annotations (controller
first-observed timestamp, trace context) to the API server. The values are
still computed in memory, so startup-latency metrics and trace propagation
within the current process keep working. The cost is losing the on-object
breadcrumb after a controller restart.

**Recommended:** enable alongside `--disable-claim-events` when squeezing
maximum throughput from the claim write path.

---

## Recommended configurations

### Sustained high-throughput profile

Traffic pattern: continuous high arrival rate (e.g. 30–100 claims/s steady).
Priority: keep the pool topped up at the arrival rate; warm latency must hold
across the entire window.

This is the configuration validated by both the live GKE A/B test and the
PR-level sustained benchmark. It provides the best and most consistent results.

```yaml
args:
  - --extensions
  # API server connections (benefit at ≥300 claims/s)
  - --api-connections=4
  - --separate-watch-connection=true
  # Concurrency
  - --sandbox-concurrent-workers=200
  - --sandbox-claim-concurrent-workers=150
  - --sandbox-warm-pool-concurrent-workers=2
  # Warm pool: rate-cap refill to smooth scheduler load; no delay
  - --sandbox-warm-pool-max-batch-size=300
  - --sandbox-warm-pool-replenish-delay=0
  - --sandbox-warm-pool-max-refill-rate=100
  # Cache (benefit on clusters with >5000 total pods)
  - --cache-label-selectors=true
  # Write reduction (benefit at ≥200 claims/s)
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

### Burst profile

Traffic pattern: many claims arrive together, then the cluster is quiet.
Priority: serve the burst at warm latency; refill can wait.

**Before enabling `replenish-delay`**, confirm warm adoption is working
(claim p50 latency in the sub-second range) and that your pool is consistently
larger than your peak burst size. See the `replenish-delay` warning above.

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
  # Warm pool: defer refill; rate-cap once it starts
  - --sandbox-warm-pool-max-batch-size=300
  - --sandbox-warm-pool-replenish-delay=20s   # ⚠️ see warning above
  - --sandbox-warm-pool-max-refill-rate=100
  # Cache and write reduction
  - --cache-label-selectors=true
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

**Sizing `replenish-delay`:** size the delay to your observed burst duration.
A 20 s delay absorbs bursts that complete within 20 s; longer bursts drain the
pool. Monitor claim p90 after enabling — a jump is the signature of pool drain.

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
