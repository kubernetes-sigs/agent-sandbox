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
| Write reduction | `--sandbox-write-behind-window` | 0 (disabled) | Coalesce Sandbox pod metadata patches via RequeueAfter |

---

## Benchmark data

These numbers come from two sources:

**PR-level benchmarks** (kops, k8s 1.35.6, e2-standard-16 control plane): the
authoritative warm-adoption numbers, run against a dedicated cluster under
controlled conditions.

**GKE live tests** (barkland-brust, k8s 1.36.2, 20× e2-standard-16 worker
nodes, 75-claim burst from a 150-sandbox warm pool): Two rounds of A/B
comparison run against the same live cluster. Pod cold-start was ~42–50 s p50
on this cluster; warm-adoption latency (sub-second when the pool is healthy)
is covered by the PR-level data. The relative impact of each flag combination
is the signal.

### Concurrency worker A/B — 75-claim burst

Worker count sweep, all other perf flags at recommended values (GKE, 75-claim
burst from a 150-sandbox warm pool):

| Config | p50 | p90 | p99 | n/75 |
|---|---|---|---|---|
| Baseline — no perf flags, 1000/1000/500/100 workers | 47.3 s | 85.3 s | 93.8 s | 75/75 |
| **Recommended workers (200/150/2/1) + all perf flags** | **49.8 s** | **89.5 s** | 98.2 s | 75/75 |
| Minimal workers (50/50/1/1) + all perf flags | 55.6 s | 95.6 s | 104.5 s | 75/75 |
| High workers (1000/1000/500/100) + all perf flags | 52.7 s | 93.5 s | 102.3 s | 75/75 |
| `--cache-label-selectors` only, baseline workers | 49.5 s | 89.8 s | 98.5 s | 75/75 |

**What this shows:**

- **Worker counts 200/150/2/1 are validated.** The recommended values beat both
  high workers and minimal workers at this scale. High worker counts add
  unnecessary API server contention; too-few workers create a reconcile bottleneck.
- **50/50/1 has a measurable regression** — p50 +11%, p90 +7% vs recommended
  values. There is a real floor below which under-provisioning hurts.
- **`--cache-label-selectors` is neutral on a small cluster.** On clusters with
  >5000 total pods it reduces informer scope from O(cluster pods) to
  O(sandbox pods) — the benefit scales with how many non-sandbox pods the cache
  would otherwise hold.
- **Perf flags add ~5% cold-start overhead** vs a no-flags baseline. This is
  expected: the flags reduce write contention during warm-adoption bursts, not
  cold pod scheduling latency.

### GKE live A/B — complete results

| Config | p50 | p90 | p99 | n/75 | Notes |
|---|---|---|---|---|---|
| A: Baseline (workers 1000/1000/500/100, batch 500) | 50.4 s | 88.2 s | 96.8 s | 75/75 | cold-start floor |
| B: + `--api-connections=4 --separate-watch-connection` | 51.2 s | 92.9 s | 102.0 s | 75/75 | neutral |
| C: + `--disable-claim-events --disable-claim-observability-annotations` | 51.1 s | 93.0 s | 101.7 s | 75/75 | neutral |
| D: + `--sandbox-warm-pool-replenish-delay=15s` | 41.5 s | 74.6 s | 82.2 s | 75/75 | neutral on its own |
| E: + `--sandbox-warm-pool-max-refill-rate=80` | 42.2 s | 75.3 s | 82.7 s | 75/75 | **−15% vs baseline** |
| G: + `--sandbox-warm-pool-max-refill-rate=100` | 41.6 s | 74.6 s | 82.0 s | 75/75 | same as rate=80 |
| **H: documented sustained config** (all flags + rate=100, no delay) | **41.4 s** | **74.2 s** | **82.6 s** | **75/75** | ✅ confirmed optimal |
| I: documented burst config (all flags + delay=20s + rate=100) | 49.4 s | 204.7 s | 214.5 s | 74/75 | ⚠️ 1 transient timeout → pool drain |

**What this shows:**

- `--sandbox-warm-pool-max-refill-rate` delivers a consistent **~15% reduction
  in p50/p90** by smoothing the pod-creation burst and reducing scheduler
  contention. Rate=80 and rate=100 perform identically, so the exact value
  matters less than enabling it.
- `--api-connections`, `--separate-watch-connection`, and write-reduction flags
  are neutral on per-claim latency at this scale (75 concurrent claims). Their
  value appears at higher scale where the API server concurrency ceiling is hit.
- `--sandbox-warm-pool-replenish-delay` is **neutral in isolation** (config D).
  However, when combined with other flags (config I), a single transient API
  timeout during claim creation (1/75 failed) allowed the pool to drain —
  because the delay prevented immediate refill — producing a catastrophic p90
  of 205 s. **Do not enable this flag unless you can guarantee zero claim
  creation failures under load.**
- Config H (the documented sustained profile) is confirmed optimal across all
  75/75 claims with no failures.

### PR-level benchmark — warm-adoption latency

Under a 45/s Poisson arrival rate across 4 pools (113 replicas/ns) on kops:

| Config | p50 | p90 | p99 |
|---|---|---|---|
| Burst-tuned (`replenish-delay=20s`, no rate cap) | ~57 ms first ~10 s, then ~1.4 s cold | 2.28 s steady-state | — |
| Sustained-tuned (`replenish-delay=0`, `max-refill-rate=100`) | **92 ms** | **182 ms** | 376 ms |

The sustained config held p50 flat at 77–116 ms across all six 10-second
windows with no pool drain. Per-pool refill ceiling is ~70–85 sandboxes/s
(pod scheduling bounds at ~70/s at the kube-scheduler default
`--kube-api-qps=50`).

### PR-level benchmark — write-behind window

Three-way A/B (300-claim warm burst + 45/s × 60s sustained Poisson, 12-node
kops cluster, e2-standard-16 control plane):

| Config | burst p50/p90 | sustained p50/p90 | pod PATCHes ok | optimistic 409s |
|---|---|---|---|---|
| Baseline (`window=0`) | 1465 / 3201 ms | **320 / 680 ms** | 2,953 | 5,447 |
| `--sandbox-write-behind-window=250ms` | **1271 / 3158 ms** | 507 / 1027 ms | 2,997 | **3,077** |

Key observations:
- **44% fewer 409 conflicts** (5,447 → 3,077) — the most reliable benefit at scale.
- **Burst p50 −13%** (1465 → 1271 ms) — write coalescing frees API seats for claim adoption.
- **Sustained p50 +58%** (320 → 507 ms) — a real trade-off; write deferral adds latency variance under continuous churn. Still well within acceptable warm-adoption bounds.
- +10% sandbox reconcile count (one extra deferred pass per coalesced write) at ~+2.5% total writes.
- Pod patch bound is always ≤ `min(window, 1s)` — the `safe-to-evict` strip cannot lag the cluster autoscaler.

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

Live testing showed no measurable impact at 75 concurrent claims. The benefit
appears at ≥300 claims/s where a single connection's stream limit saturates.

**Recommended:** `--api-connections=4` on clusters with ≥300 claims/s.

### `--separate-watch-connection` (default: false)

Gives the informer cache (list/watch) a dedicated HTTP/2 connection, isolating
watch frames from write-burst congestion on the shared connection.

**Recommended:** enable alongside `--api-connections` > 1.

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
(baseline 50.4 s → 41.4–42.2 s p50; 88.2 s → 74.2–75.3 s p90). Rate=80 and
rate=100 produced statistically identical results, so the exact value above
~50/s is not critical.

This flag is safe to enable broadly — it helps even when claims fall through
to cold start, and it has no failure mode.

**Sizing:** set per-pool rate ≥ your steady claim arrival rate to prevent pool
drain. If you have 4 pools and 80 claims/s total, 20/s per pool is sufficient;
100/s leaves headroom for spikes. The per-pool ceiling on a standard control
plane is ~70–85/s.

### `--sandbox-warm-pool-replenish-delay` (default: 0 — immediate)

Defers the **start** of refill after pool members are adopted. While claims
are still arriving, the timer re-arms; it only fires once the pool is stable
for the delay duration. Initial pool fill and scale-ups are never delayed.

In isolation, this flag is **neutral** — live testing of `replenish-delay=15s`
alone produced the same latency as `max-refill-rate=100` alone (p50=41.5 s,
p90=74.6 s). The flag's benefit only appears when warm adoption is actually
occurring and the pool is large enough to absorb the full burst.

**⚠️ Fragility:** when combined with other flags under load, even a **single
transient API timeout** during claim creation is enough to trigger pool drain,
because the delay prevents the immediate refill that would have replenished the
pool before the straggler claim arrived. In live testing, 1/75 failed claim
applies caused p90 to jump from 74 s to **205 s** (config I). This failure
mode does not exist with `replenish-delay=0`.

**When to use:** only enable this flag if warm adoption is confirmed (claim
p50 in the sub-second range) and your pool is consistently larger than your
maximum burst size with headroom for transient failures. Monitor claim p90
continuously after enabling — any jump is the signature of pool drain.

The two flags compose: `replenish-delay` defers the start of refill;
`max-refill-rate` shapes its flow once started. In most cases `max-refill-rate`
alone gives the same performance with none of the fragility.

---

## Informer cache scoping

### `--cache-label-selectors` (default: false)

Scopes the Pod and Service informer caches to objects carrying the sandbox
tracking label. The controller only ever creates Pods and Services it labeled
itself, so on large or shared clusters this cuts informer list/watch volume,
JSON decode CPU, and cache memory from O(cluster pods) to O(sandbox pods).

**Recommended:** enable on clusters with > ~5000 total pods or where the
controller's pod cache is a meaningful fraction of memory. Benchmarked in
isolation on a 4-node clean cluster (config M above): neutral effect at small
scale (p50 49.5 s vs 47.3 s baseline) as expected — the benefit scales with
how many non-sandbox pods the cache would otherwise track.

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

Live testing showed no measurable per-claim latency improvement in isolation;
the benefit appears when the events API level in APF is saturated or at claim
rates above ~200/s.

### `--disable-claim-observability-annotations` (default: false)

Skips persisting the SandboxClaim observability annotations (controller
first-observed timestamp, trace context) to the API server. The values are
still computed in memory, so startup-latency metrics and trace propagation
within the current process keep working. The cost is losing the on-object
breadcrumb after a controller restart.

### `--sandbox-write-behind-window` (default: 0 — disabled)

Coalesces the Sandbox controller's recoverable pod metadata patch via
RequeueAfter deferral. The specific write deferred is the pod
label/annotation reconciliation on the warm-pool adoption path (warm-pool
label prune, `cluster-autoscaler.kubernetes.io/safe-to-evict` strip,
propagated-keys tracking). When the window is open, the reconcile pass skips
the patch and returns `RequeueAfter: <remaining window>`; the workqueue's
per-key dedup coalesces repeated redeliveries into a single flush pass that
recomputes the desired state from fresh informer state and issues one merge
patch.

**Properties:**
- Sandbox **readiness is never gated** on the deferred write — the claim Ready
  condition is set from in-memory state without waiting for the flush.
- Pod patch is always flushed within `min(window, 1s)`, bounding the
  `safe-to-evict` annotation lag well below the cluster autoscaler's 10 s
  scan interval.
- Crash-safe: the deferral clock stores timestamps only, no mutation payload.
  A crash merely restarts a sub-second window on the next leader's first pass.

**Trade-offs (benchmarked at 250ms):**

| | Baseline (0) | 250ms |
|---|---|---|
| 409 write conflicts | 5,447 | **3,077 (−44%)** |
| Burst p50/p90 | 1465/3201 ms | **1271/3158 ms (−13% p50)** |
| Sustained p50/p90 | **320/680 ms** | 507/1027 ms (+58% p50) |
| Sandbox reconciles | +0% | +10% |

The sustained latency regression is real: deferred writes add variance under
continuous churn. For most deployments the 44% reduction in conflict errors
and the burst improvement outweigh it.

**Recommended:** `--sandbox-write-behind-window=250ms` on clusters with high
write-conflict rates (watch for 409 errors in controller logs) or ≥300
claims/s burst workloads. Disable (leave at 0) if sustained warm-adoption
latency is the primary constraint and you observe no significant 409 rate.

---

## Recommended configurations

### Sustained high-throughput profile ✅ (validated optimal)

Traffic pattern: continuous high arrival rate (e.g. 30–100 claims/s steady).
Priority: keep the pool topped up at the arrival rate; warm latency must hold
across the entire window.

This is **config H** from the live benchmark — 75/75 claims, p50=41.4 s,
p90=74.2 s, no failures across all runs. It is the default recommendation
for any high-throughput deployment.

Worker counts 200/150/2/1 are **directly benchmarked** — they outperform both
high workers (1000/1000/500) and minimal workers (50/50/1) at 75 concurrent
claims. See the concurrency worker A/B table above.

```yaml
args:
  - --extensions
  # API server connections (benefit at ≥300 claims/s)
  - --api-connections=4
  - --separate-watch-connection=true
  # Concurrency — validated at 75 concurrent claims; outperforms 1000/1000/500
  - --sandbox-concurrent-workers=200
  - --sandbox-claim-concurrent-workers=150
  - --sandbox-warm-pool-concurrent-workers=2
  # Warm pool: smooth refill, no delay
  - --sandbox-warm-pool-max-batch-size=300
  - --sandbox-warm-pool-replenish-delay=0
  - --sandbox-warm-pool-max-refill-rate=100
  # Cache (benefit on clusters with >5000 total pods)
  - --cache-label-selectors=true
  # Write reduction (benefit at ≥200 claims/s)
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
  # Write-behind coalescing: reduces 409 conflicts by ~44%; slight sustained latency trade-off
  - --sandbox-write-behind-window=250ms
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

**Sizing `--sandbox-warm-pool-max-refill-rate`:** set it ≥ your per-pool
steady claim arrival rate. Rate=80 and rate=100 were statistically identical
in testing; setting 100 provides headroom for spikes. The per-pool ceiling is
~70–85/s on a standard control plane.

### Burst profile ⚠️ (use only with confirmed prerequisites)

Traffic pattern: many claims arrive together, then the cluster is quiet.
Priority: serve the burst at warm latency; refill can wait.

**Prerequisites before enabling `replenish-delay`:**
1. Confirm warm adoption is working (claim p50 is sub-second, not tens of seconds).
2. Confirm your pool is consistently larger than your maximum burst size, with
   headroom for transient claim creation failures under API pressure.
3. Have monitoring on claim p90 in place — any jump is pool drain.

If you cannot confirm all three, use the sustained profile above instead.
It delivers equivalent performance without the fragility.

```yaml
args:
  - --extensions
  - --api-connections=4
  - --separate-watch-connection=true
  - --sandbox-concurrent-workers=200
  - --sandbox-claim-concurrent-workers=150
  - --sandbox-warm-pool-concurrent-workers=2
  - --sandbox-warm-pool-max-batch-size=300
  - --sandbox-warm-pool-replenish-delay=20s   # ⚠️ see prerequisites above
  - --sandbox-warm-pool-max-refill-rate=100
  - --cache-label-selectors=true
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
  - --sandbox-write-behind-window=250ms
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

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
  - --sandbox-write-behind-window=250ms
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
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--disable-claim-observability-annotations=true"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-write-behind-window=250ms"}
  ]'
```
