# Fleet launch benchmark: a million sandboxes in a minute

This scenario measures how fast a *fleet* of Kubernetes clusters can launch
Sandboxes — the [`agents.x-k8s.io`](https://github.com/kubernetes-sigs/agent-sandbox)
`Sandbox` custom resource — and drive them to `Ready`.

**Result so far:** **1,037,564 Sandboxes `Ready` inside a fixed 60-second
window**, across **20 clusters** running **pure upstream Kubernetes** (one
default `kube-scheduler` per cluster, no custom scheduling), with **0 create
errors**. The verdict is computed from the server-side `Ready` condition
timestamps, over a window fixed at `[firstReady+10s, firstReady+70s)` — it
does not slide to find a peak, so it cannot flatter the number.

## How it works

One Go driver (`test/fleet`) talks to N clusters at once:

- It creates Sandboxes across the fleet at a paced aggregate rate. Each
  Sandbox's ordinal is hashed to a cluster (`fnv(id) % N`) — a deliberately
  non-uniform, real-world-ish distribution, not a perfect round-robin.
- Each cluster gets its **own rate limiter** at `rate / N` and its **own
  worker pool**. A single shared limiter is unfair under wide-area latency
  (near clusters' workers grab a disproportionate share of tokens and starve
  far clusters), which staggers the per-cluster readiness peaks so the fleet's
  single verdict window can't capture them together.
- It watches **only Sandboxes** (never Pods). A pod firehose from N clusters
  would make the driver itself the bottleneck. On a `410 Gone`
  ("too old resource version", routine at fleet churn) the watch does a
  reflector-style LIST resync rather than re-watching from "now", so it never
  silently drops `Ready` transitions.
- Artifacts are per-cluster gzipped JSONL (`creates-*.jsonl.gz`,
  `ready-*.jsonl.gz`) plus `summary.json`. **The raw JSONL is the source of
  truth**; the report and verdict are recomputed from it.

## Prerequisites

- A GCP project with billing enabled, and `gcloud` authenticated to it.
- `go`, `python3`, and `kubectl`. `kOps` is installed automatically
  (pinned version) into `~/.fleet-kops/bin`.
- Per-**region** quota (these are the ones that bite at scale):
  - `N2_CPUS` — each cluster is ~816 vCPU (`200 x n2-standard-4` workers +
    an `n2-standard-16` controller node); a control plane adds 64–88 more.
  - `C3_CPUS` — 88 per `c3-standard-88` control plane (see substitution note
    below).
  - `SSD_TOTAL_GB` — 40,960 by default. Worker boot disks are **100 GB**
    (not the kOps default 128): two clusters sharing a region at 128 GB blow
    the SSD quota at ~160 nodes each; 100 GB fits `2 x 200 x 100 = 40,000`.
  - Machine **stock** is separate from quota and drifts hourly. `c3-standard-88`
    in particular is often exhausted in a zone — probe before you commit
    (`gcloud compute instances create ... --machine-type=c3-standard-88`,
    then delete), and fall back to `n2-standard-64` (see below).

## Run it

Three scripts, each idempotent:

```bash
cd test/benchmarks/scenarios/fleet-kops-gcp

# 1. Bring up the clusters (one per zone in FLEET_ZONES).
FLEET_CLUSTER_COUNT=2 \
FLEET_ZONES=us-east4-b,us-central1-b \
FLEET_NODE_SIZE=n2-standard-4 \
FLEET_CONTROL_PLANE_SIZE=c3-standard-88 \
./up

# 2. Drive the launch test and generate the HTML report.
FLEET_COUNT=200000 FLEET_RATE=2000 ./run

# 3. Tear everything down (stops billing).
./down
```

The full 20-cluster million-sandbox run used `FLEET_COUNT=1440000`
`FLEET_RATE=20000` (~1,000/s per cluster — the sweet spot; pushing harder
*lowers* the result, see below) across 20 zones spread over the US, Europe,
Asia, and Australia.

Key environment knobs:

| Script | Variable | Default | Notes |
|---|---|---|---|
| `up` | `FLEET_ZONES` | `us-east4-b,us-central1-b` | one cluster per zone; clusters are named `fleet-<zone>` |
| `up` | `FLEET_NODE_SIZE` | `n2-standard-8` | use `n2-standard-4`: holds 512 pods/node at the same rate, half the N2 quota |
| `up` | `FLEET_CONTROL_PLANE_SIZE` | `n2-standard-4` | use `c3-standard-88` (or `n2-standard-64`); the apiserver/etcd write path is the per-cluster ceiling |
| `up` | `FLEET_CONTROLLER_NODE_SIZE` | `n2-standard-16` | dedicated, tainted node for the sandbox controller |
| `up` | `FLEET_MAX_PODS` | `500` | keep below the `/23` pod-CIDR usable-IP count (~509) |
| `up` | `FLEET_NODE_VOLUME_SIZE` | `100` | worker boot disk GB (SSD quota; see above) |
| `run` | `FLEET_COUNT` / `FLEET_RATE` | `2000` / `200` | total Sandboxes / aggregate creates per second |
| `run` | `FLEET_SKIP_{CLEANUP,PREPULL_CHECK,CAPACITY_CHECK}` | – | skip pre-flights (e.g. capacity check for a heterogeneous fleet) |
| `run` | `FLEET_EXTRA_ARGS` | – | passed to the driver, e.g. `--pprof-addr=:6060` |

Driver flags of note (set via `FLEET_EXTRA_ARGS`): `--create-concurrency`
(default 512 — sized so the farthest clusters can still reach their
per-cluster rate; a worker blocks one round trip per create, so sustaining
`R/s` at `RTT` needs ~`R*RTT` workers) and `--conns-per-cluster` (default 8,
must cover create-concurrency in HTTP/2 streams).

## Reading the report

`run` writes `report.html` into the artifacts directory and updates the
`fleet-report-latest.html` symlink. It shows the verdict tiles (the fixed
60s window is the headline; sliding-best is reference), aggregate created/s
vs ready/s over time, and per-cluster small multiples. Everything is
recomputed from the raw JSONL, and a mismatch against `summary.json` is
rendered as a loud banner rather than silently reconciled.

## What actually moves the number (hard-won)

- **The verdict is readiness-bound, not driver-bound.** Once the driver isn't
  artificially throttled, the fleet result is set by how fast the clusters
  turn Pending pods into `Ready` ones.
- **Each cluster tops out at ~54k `Ready`/min**, limited by its single
  apiserver + etcd write pipeline — *not* the scheduler (which sits ~40%
  idle). This ceiling is per-cluster and **additive**: scaling is linear
  (measured 2.01x at 2 clusters, 5.01x at 5), so you cross 1M/min by adding
  clusters, not by tuning one harder.
- **Feed each cluster ~1,000/s; faster is worse.** Pushing the aggregate rate
  up write-saturates the apiservers, and the extra create load steals capacity
  from the readiness pipeline — creates *and* readiness both slow down.
- **Controller tuning matters:** `--sandbox-concurrent-workers=400` plus the
  status-write dampeners (`--sandbox-transitional-status-window`,
  `--sandbox-write-behind-window`) on a large dedicated node lifted per-cluster
  readiness substantially over the defaults.
- **`n2-standard-64` is a fine control-plane substitute** when `c3-standard-88`
  is out of stock — such clusters were top performers in the 20-cluster run.
- **Sandbox pod template:** `automountServiceAccountToken: false` (avoids a
  TokenRequest round trip per pod), `restartPolicy: Never`,
  `terminationGracePeriodSeconds: 0`, and `imagePullPolicy: IfNotPresent` with
  a pre-pulled image — so the run measures launches, not registry pulls or
  incidental control-plane traffic.

## Scaling to your own target

Rough sizing: `clusters ~= ceil(target_per_min / 54000)`, one cluster per
zone, `~1,000/s` aggregate feed per cluster. Watch the three regional quotas
(`N2_CPUS`, `C3_CPUS`, `SSD_TOTAL_GB`) and probe `c3` stock before bring-up.
The pre-flight in `run` (capacity check) will abort loudly rather than let a
run discover a shortfall in the readiness curve.
