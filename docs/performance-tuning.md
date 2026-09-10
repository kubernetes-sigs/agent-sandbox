# Performance Tuning

Benchmark data and sizing rationale for running `agent-sandbox-controller`
under high claim rates.

[Configuration](configuration.md) is the canonical flag reference — every
flag named here is defined there, and its
[High-Throughput & Scale Tuning](configuration.md#high-throughput--scale-tuning)
section has a quick-start Deployment manifest. This guide adds what the
reference deliberately leaves out: the benchmark evidence behind those
recommendations, how to size each knob from your own traffic profile, and
where each knob helps, is neutral, or can hurt.

All flags are on the `agent-sandbox-controller` binary. Defaults are safe for
small deployments; everything below is opt-in. Flags not covered here
(readiness grace period, unschedulable recheck interval, warm pool eviction,
pprof) are in the [flag reference](configuration.md).

---

## Before tuning: confirm warm adoption applies

Most latency numbers below concern **warm adoption** — a `SandboxClaim`
binding a pre-created sandbox from a `SandboxWarmPool`. Two claim fields
disable warm adoption entirely: a claim that sets `spec.env` or
`spec.volumeClaimTemplates` always cold-starts a fresh sandbox from the
pool's template, even when the pool has ready capacity, because those
customizations cannot be applied to an already-running warm pod.
(`spec.additionalPodMetadata` does not have this effect — it is applied to
the adopted sandbox in place.) If your claims set either field, pool sizing
and refill shaping will not improve their latency.

---

## Benchmark data

These numbers come from two sources:

**PR-level benchmarks** (kops, k8s 1.35.6, e2-standard-16 control plane): the
authoritative warm-adoption numbers, run against a dedicated cluster under
controlled conditions.

**GKE live tests** (k8s 1.36.2, 20× e2-standard-16 worker nodes, 75-claim
burst from a 150-sandbox warm pool): two rounds of A/B comparison run against
the same live cluster. Pod cold-start was ~42–50 s p50 on this cluster;
warm-adoption latency (sub-second when the pool is healthy) is covered by the
PR-level data. The relative impact of each flag combination is the signal.

### Concurrency worker A/B — 75-claim burst

Worker count sweep, all other perf flags at recommended values (GKE, 75-claim
burst from a 150-sandbox warm pool). Worker quadruples are
`sandbox/claim/warm-pool/template`:

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
- **50/50/1/1 has a measurable regression** — p50 +11%, p90 +7% vs recommended
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
| **H: sustained profile below** (all flags + rate=100, no delay) | **41.4 s** | **74.2 s** | **82.6 s** | **75/75** | ✅ confirmed optimal |
| I: burst profile below (all flags + delay=20s + rate=100) | 49.4 s | 204.7 s | 214.5 s | 74/75 | ⚠️ 1 transient timeout → pool drain |

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
- Config H (the sustained profile below) is confirmed optimal across all
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

Two-configuration A/B (300-claim warm burst + 45/s × 60s sustained Poisson,
12-node kops cluster, e2-standard-16 control plane):

| Config | burst p50/p90 | sustained p50/p90 | pod PATCHes ok | optimistic 409s |
|---|---|---|---|---|
| Baseline (`window=0`) | 1465 / 3201 ms | **320 / 680 ms** | 2,953 | 5,447 |
| `--sandbox-write-behind-window=250ms` | **1271 / 3158 ms** | 507 / 1027 ms | 2,997 | **3,077** |

Key observations:
- **44% fewer 409 conflicts** (5,447 → 3,077) — the most reliable benefit at scale.
- **Burst p50 −13%** (1465 → 1271 ms) — write coalescing frees API seats for claim adoption.
- **Sustained p50 +58%** (320 → 507 ms, p90 680 → 1027 ms) — a significant
  regression under continuous churn; see the
  [write-behind section](#--sandbox-write-behind-window-default-0--disabled)
  for when the trade-off is worth it.
- +10% sandbox reconcile count (one extra deferred pass per coalesced write) at ~+2.5% total writes.
- Pod patch bound is always ≤ `min(window, 1s)` — the `safe-to-evict` strip cannot lag the cluster autoscaler.

---

## API connection tuning

Flag reference:
[API Transport and Connection Settings](configuration.md#api-transport-and-connection-settings).

The kube-apiserver caps concurrent in-flight HTTP/2 streams per connection
(100 by default, configurable server-side via
`--http2-max-streams-per-connection`), so a single connection bounds the
controller's effective request concurrency regardless of worker count.
`--api-connections=N` shards non-watch traffic (writes, uncached reads,
events, leader election) round-robin across N dedicated connections;
`--separate-watch-connection` isolates the informer watch streams from
write-burst congestion. Enable them together.

**When they matter:** live testing showed both are neutral on per-claim
latency at 75 concurrent claims. The benefit appears when concurrent
in-flight requests approach the per-connection stream limit (~100) — i.e.
large worker counts with sustained bursts — where a shared connection also
starves watch delivery and can wedge warm-pool refill on the expectations
gate (see
[High-Throughput & Scale Tuning](configuration.md#high-throughput--scale-tuning)).

**`--kube-api-qps` / `--kube-api-burst`:** the default QPS of -1 means no
client-side throttle — the server's APF policy controls admission instead,
which is the preferred approach (see [APF insulation](#api-priority-and-fairness-apf-insulation)
below). If you must apply client-side throttling, set `--kube-api-burst` ≥
the total worker count (`sandbox + claim + warm-pool + template`); the
controller logs a startup warning when it detects a positive QPS with a burst
below the worker total.

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
| `--sandbox-warm-pool-concurrent-workers` | Multiple warm pools | Size to the number of active pools — reconciles for one pool are serialized, so extra workers only help across distinct pools |
| `--sandbox-template-concurrent-workers` | Many templates changing frequently | rarely > 2 |

Worker counts 200/150/2/1 are **directly benchmarked** — they outperform both
high workers (1000/1000/500/100) and minimal workers (50/50/1/1) at 75
concurrent claims (see the worker A/B table above). The benchmark ran a
single pool; per the sizing rule, `--sandbox-warm-pool-concurrent-workers=1`
is equivalent for one pool.

**Warning:** `sandbox + claim + warm-pool + template` workers totaling > 1000
triggers a startup warning. The practical ceiling is usually the API server's
mutating inflight limit, not the worker count.

---

## Warm pool refill shaping

When a burst of SandboxClaims adopts warm sandboxes, the warm-pool controller
immediately tries to refill the deficit. Without shaping, this refill burst
competes for API server write capacity with the very claim-adoption writes it
is meant to support. Flag reference:
[Warm Pool Replenishment Shaping](configuration.md#warm-pool-replenishment-shaping).

### `--sandbox-warm-pool-max-batch-size` (default: 300)

Maximum sandboxes created per reconcile round. Creates advance one observed
batch per watch round-trip (the expectations gate waits for a batch's `ADD`
events before issuing the next), so a refill deficit of `D` sandboxes clears
in about `ceil(D / batchSize)` round-trips — an initial pool fill is the
`D = replicas` case. Raising it trades round-trips for burst size and is safe
at any value under the gate. When a refill rate cap is set (below), the token
bucket usually bounds each batch before the batch cap does.

### `--sandbox-warm-pool-max-refill-rate` (default: 0 — unpaced)

Caps the refill rate in sandboxes/second per pool via a per-pool token
bucket. Turns full-deficit burst creates into a smooth stream, reducing
scheduler contention. When no token is available the controller requeues for
exactly when the next token accrues; partial batches advance on the watch
events of their own creates. The cap paces **all** pool creates, including
initial fill and scale-ups — live verification of a 150-replica pool at
rate=100 filled in two paced batches (100, then 50 a second later).

Live testing confirmed a consistent **~15% reduction in p50 and p90 latency**
(baseline 50.4 s → 41.4–42.2 s p50; 88.2 s → 74.2–75.3 s p90). Rate=80 and
rate=100 produced statistically identical results, so the exact value above
~50/s is not critical.

This flag is safe to enable broadly — it helps even when claims fall through
to cold start, and it has no failure mode.

**Sizing:** set per-pool rate ≥ your steady per-pool claim arrival rate to
prevent pool drain. If you have 4 pools and 80 claims/s total, 20/s per pool
is sufficient; 100/s leaves headroom for spikes. The per-pool ceiling on a
standard control plane is ~70–85/s. (The
[quick-start manifest](configuration.md#high-throughput--scale-tuning) uses
rate=25, sized for a ~20 claims/s workload by the same rule.)

### `--sandbox-warm-pool-replenish-delay` (default: 0 — immediate)

Defers the **start** of refill after pool members are adopted. While claims
are still arriving, the timer re-arms; it only fires once the pool is stable
for the delay duration. Initial pool fill and scale-ups are never delayed by
this flag (though the rate cap above, if set, still paces them).

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
tracking label (`agents.x-k8s.io/sandbox-name-hash`). The controller only
ever creates Pods and Services it labeled itself, so on large or shared
clusters this cuts informer list/watch volume, JSON decode CPU, and cache
memory from O(cluster pods) to O(sandbox pods).

**Recommended:** enable on clusters with > ~5000 total pods or where the
controller's pod cache is a meaningful fraction of memory. Benchmarked in
isolation on a small clean cluster (worker A/B table above): neutral at small
scale (p50 49.5 s vs 47.3 s baseline) as expected — the benefit scales with
how many non-sandbox pods the cache would otherwise track.

**Caveat:** externally pre-provisioned resources that rely on the
`agents.x-k8s.io/adoptable=true` adoption path must also carry the tracking
label (value = the owning sandbox's name hash) to remain visible to the
controller when this flag is enabled.

---

## Write reduction

During a claim burst, every avoidable write competes for API server capacity
with latency-critical adoption writes. Flag reference:
[API Write and Cache Optimization](configuration.md#api-write-and-cache-optimization).

### `--disable-claim-events` (default: false)

Disables Kubernetes Event emission from the SandboxClaim controller. Events
are informational only; removing them eliminates roughly one write per claim
lifecycle transition.

Live testing showed no measurable per-claim latency improvement in isolation;
the benefit appears when the events API level in APF is saturated or at claim
rates above ~200/s.

### `--disable-claim-observability-annotations` (default: false)

Skips the dedicated API write that persists the SandboxClaim observability
annotations (controller first-observed timestamp, trace context). The values
are still stamped on the in-memory object, so startup-latency metrics and
trace propagation within the current process keep working.

Treat this flag as removing one write per claim, not as a guarantee the
annotations never appear on the object: when a later full-object update
carries the claim (e.g. the adoption update on the warm path), the stamped
values are persisted along with it. The write saving holds either way.

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

The sustained regression is significant: +58% p50 (320 → 507 ms) and +51% p90
(680 → 1027 ms) under continuous churn.

**Recommended:** enable `--sandbox-write-behind-window=250ms` only when 409
write conflicts are high (watch for them in controller logs) or the workload
is burst-dominated, **and** your latency SLO tolerates ~500 ms sustained
warm-adoption p50. Leave it at 0 when sustained warm-adoption latency is the
primary constraint — which is why the sustained profile below omits it.

---

## Benchmark-validated configurations

These are the exact configurations validated in the GKE live A/B above. The
[quick-start manifest in configuration.md](configuration.md#high-throughput--scale-tuning)
is a fine starting point for moderate sustained workloads (~20 claims/s,
single pool); the profiles below show how the same sizing rules apply at
benchmark scale, and what to change for burst-dominated traffic.

### Sustained high-throughput profile ✅ (validated optimal)

Traffic pattern: continuous high arrival rate (e.g. 30–100 claims/s steady).
Priority: keep the pool topped up at the arrival rate; warm latency must hold
across the entire window.

This is **config H** from the live benchmark — 75/75 claims, p50=41.4 s,
p90=74.2 s, no failures across all runs. It is the default recommendation
for any high-throughput deployment.

```yaml
args:
  - --extensions
  # API server connections (benefit when in-flight requests approach the
  # ~100-stream per-connection limit)
  - --api-connections=4
  - --separate-watch-connection=true
  # Concurrency — validated at 75 concurrent claims; outperforms 1000/1000/500/100.
  # Size warm-pool workers to the number of active pools (benchmark: single pool).
  - --sandbox-concurrent-workers=200
  - --sandbox-claim-concurrent-workers=150
  - --sandbox-warm-pool-concurrent-workers=2
  # Warm pool: smooth refill, no delay (size rate ≥ per-pool claim arrival rate)
  - --sandbox-warm-pool-replenish-delay=0
  - --sandbox-warm-pool-max-refill-rate=100
  # Cache (benefit on clusters with >5000 total pods)
  - --cache-label-selectors=true
  # Write reduction (benefit at ≥200 claims/s)
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
  # NOT set: --sandbox-write-behind-window — costs +58% sustained p50; enable
  # only for burst-dominated traffic or high 409 rates (see Write reduction)
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
  - --sandbox-warm-pool-replenish-delay=20s   # ⚠️ see prerequisites above
  - --sandbox-warm-pool-max-refill-rate=100
  - --cache-label-selectors=true
  - --disable-claim-events=true
  - --disable-claim-observability-annotations=true
  - --sandbox-write-behind-window=250ms   # burst-dominated traffic: −13% burst p50, −44% 409s
```

Also apply `examples/apf-insulation/apf-insulation.yaml` to the cluster.

---

## Applying settings

For a complete tuned Deployment manifest (selector, service account, image),
see the
[High-Throughput Deployment Example](configuration.md#high-throughput--scale-tuning)
in the flag reference. The warm-pool and claim flags only take effect on a
deployment running with `--extensions`.

**Live cluster patch:** use a strategic merge patch that targets the
container **by name** and replaces the whole `args` list. Unlike appending
individual args with a JSON patch, this is idempotent (re-running it cannot
duplicate flags) and does not assume the controller is at container index 0:

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type=strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{
    "name":"agent-sandbox-controller",
    "args":[
      "--leader-elect=true",
      "--extensions",
      "--api-connections=4",
      "--separate-watch-connection=true",
      "--sandbox-concurrent-workers=200",
      "--sandbox-claim-concurrent-workers=150",
      "--sandbox-warm-pool-concurrent-workers=2",
      "--sandbox-warm-pool-max-refill-rate=100",
      "--cache-label-selectors=true",
      "--disable-claim-events=true",
      "--disable-claim-observability-annotations=true"
    ]}]}}}}'
```

Because the patch replaces the full list, include every flag the deployment
needs (including `--leader-elect` and `--extensions` shown above), not just
the tuning flags. Check the current list first with:

```bash
kubectl get deployment agent-sandbox-controller -n agent-sandbox-system \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="agent-sandbox-controller")].args}'
```
