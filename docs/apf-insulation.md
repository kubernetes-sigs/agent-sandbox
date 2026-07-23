# APF insulation for high-rate claim workloads

An opt-in overlay of [API Priority and Fairness (APF)](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
objects that gives the agent-sandbox controller dedicated apiserver
concurrency: [`examples/apf-insulation/apf-insulation.yaml`](../examples/apf-insulation/apf-insulation.yaml).

**This overlay changes nothing unless a cluster operator applies it.** It is
not part of the default or release manifests. Whether APF defaults belong in
the standard install is a cluster-operator decision: the right seat sizing
depends on the server's inflight limits, the cluster's other tenants, and
the target claim rate — there is no universally safe default.

**What to expect:** this is an isolation and capacity-guarantee mechanism,
**not a latency optimization at typical rates**. On a single-tenant cluster
whose apiserver seats are not saturated, APF queueing wait is already ~0 and
applying the overlay does not change request latency — measured A/B, the
controller's traffic simply moves into its dedicated levels with no latency
delta. The value is what happens under contention: other tenants can no
longer queue the claim path, the controller's own refill bursts can no
longer crowd out its adoption writes, and the claim path keeps guaranteed
seats when the cluster is busy.

## The problem: everything shares `workload-low`

Out of the box, all of the controller's API traffic matches the built-in
`service-accounts` FlowSchema (matchingPrecedence 9000) and is served by the
shared `workload-low` priority level, together with every other
non-kube-system ServiceAccount in the cluster. At high SandboxClaim rates
this produces two distinct failure modes:

1. **Cross-tenant interference.** Any other workload's ServiceAccount can
   flood `workload-low` and queue the controller's claim-adoption writes
   behind its traffic.
2. **Self-interference.** The controller's own bulk traffic — warm-pool
   replenishment creating hundreds of sandboxes, pods and services right
   after a claim burst drains the pool — competes for the same seats as the
   latency-critical adoption writes.

The overlay splits the controller's traffic into three classes, in strict
priority order: **claim path > bulk refill > events**.

| class | APF objects | contents |
|---|---|---|
| claim path | `agent-sandbox-critical` (PriorityLevelConfiguration + FlowSchema, precedence 900) | SandboxClaim writes and status, sandbox adoption update/patch and status, the synchronous adoption-time pod patch, leader-election leases |
| bulk refill | `agent-sandbox-bulk` (PriorityLevelConfiguration + FlowSchema, precedence 1000) | everything else: refill creates/deletes, informer list/watch, child objects, discovery |
| events | `agent-sandbox-events` (FlowSchema only, precedence 950) | controller Events, routed to the pre-existing shared `workload-low` level — sacrificial by design |

## How seats are derived (and why the manifest has no absolute numbers)

APF seat counts are **fractions**, not absolutes:

```
seats(level) = shares(level) / sum(all levels' shares)
               x (--max-requests-inflight + --max-mutating-requests-inflight)
```

On Kubernetes 1.35 the built-in mandatory + suggested levels sum to 245
nominal concurrency shares. The overlay adds `critical = 40` and
`bulk = 25`, bringing the total to 310:

* `agent-sandbox-critical`: 40/310 ≈ 13% of the server's seats.
* `agent-sandbox-bulk`: 25/310 ≈ 8%.

Because the shares are fractions, **raising the server-wide inflight limits
multiplies every level's seats with no manifest change**. For example, with
the apiserver defaults (`--max-requests-inflight=400`,
`--max-mutating-requests-inflight=200`, i.e. 600 seats total) the critical
level gets ~77 seats; on a control plane tuned to 3000 read + 1000 mutating
(4000 seats total — e.g. raising mutating inflight 200 → 1000 alongside the
read limit) the same manifest yields ~516 critical seats and ~322 bulk
seats. Fractions are preserved; only the server limits move.

## The sizing rule: `seats(critical) >= 2x demand high watermark`

Dedicated seats only help if there are enough of them — an undersized
dedicated level becomes the queueing point itself.

*Historical context (measured during the latency investigation, at
pre-optimization controller write volumes):* on a 300-claim
simultaneous-burst benchmark, with the server at the 600-seat defaults, the
critical level's ~77 seats were exceeded by a measured **272-seat demand
high watermark**, producing an APF wait p99 of ~359ms *inside the
controller's own priority level*; raising the server limits to 4000 seats
(critical ≈ 516) put the level back above demand with ~2x headroom, and the
burst p90 improved 1740ms → 1094ms with no manifest change. That experiment
is why the sizing rule below exists — but note it reflects a controller
that issued several times more writes per claim than the current one.
**Current optimized write volumes do not saturate the stock 600-seat limits
at a 45/s sustained claim rate** (measured demand high watermarks: 86 seats
critical, 52 bulk), which is exactly why the overlay is protection for
multi-tenant and higher-rate regimes rather than a latency optimization at
typical rates.

Rule of thumb: at your **peak** claim rate,

```
seats(agent-sandbox-critical) >= 2 x demand-seats high watermark
```

and fix a shortfall by raising the server inflight limits (which scales
every level proportionally), not by editing the shares.

## How to measure

All metrics below are served by the kube-apiserver (`/metrics`); they carry
a `priority_level` label once the overlay is applied.

**Demand high watermark** (peak seats the level *wanted* during each
sampling window — compare against the level's seat count):

```promql
max_over_time(
  apiserver_flowcontrol_demand_seats_high_watermark{priority_level="agent-sandbox-critical"}[1h]
)
```

Run your peak workload (e.g. a full-scale claim burst) inside the range
window. Repeat with `priority_level="agent-sandbox-bulk"`.

**Current seat allocation** (what the fractions currently resolve to):

```promql
apiserver_flowcontrol_nominal_limit_seats{priority_level=~"agent-sandbox-.*"}
```

**APF wait p99** (time requests spent queued at the level before executing —
the direct "is my level big enough" signal; healthy is single-digit
milliseconds):

```promql
histogram_quantile(0.99, sum by (le) (
  rate(apiserver_flowcontrol_request_wait_duration_seconds_bucket{
    priority_level="agent-sandbox-critical", execute="true"
  }[5m])
))
```

**Rejections** (must stay zero — both levels queue rather than 429, so any
rejection means the queue length limit was exhausted):

```promql
sum by (priority_level) (
  rate(apiserver_flowcontrol_rejected_requests_total{priority_level=~"agent-sandbox-.*"}[5m])
)
```

**Verify the schemas actually match** (non-zero dispatch on both dedicated
levels while the controller is busy):

```promql
sum by (priority_level) (
  rate(apiserver_flowcontrol_dispatched_requests_total{priority_level=~"agent-sandbox-.*"}[5m])
)
```

## Rollback

```sh
kubectl delete -f examples/apf-insulation/apf-insulation.yaml
```

Deletion is immediate and safe: the controller's traffic falls back to the
built-in `service-accounts` FlowSchema (shared `workload-low`), which is the
stock behavior.

## Caveats

* **Opt-in by design.** The default and release manifests do not include
  these objects; applying them is a deliberate cluster-operator action, and
  the seat math above should be re-derived for your server limits and claim
  rate before relying on the overlay in production.
* **Third-party priority levels change the denominator.** The share
  fractions assume the stock built-in levels (245 shares). Other add-ons
  that install PriorityLevelConfigurations shift every level's fraction;
  re-check `apiserver_flowcontrol_nominal_limit_seats` after installing any.
* **Clients with `system:masters` bypass APF entirely** (they match the
  mandatory `exempt` FlowSchema). If you load-test with an admin kubeconfig,
  the test client's requests are not shaped by this overlay — check
  `kubectl auth whoami` before drawing conclusions from a benchmark.
* **Events are deliberately sacrificial.** If you rely on controller Events
  for alerting at high rates, be aware they share `workload-low` with the
  rest of the cluster under this overlay.
