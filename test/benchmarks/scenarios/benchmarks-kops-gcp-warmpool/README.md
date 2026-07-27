# benchmarks-kops-gcp-warmpool

An **optional, manually triggered** scenario that verifies the
SandboxWarmPool controller's fill-time invariants against a real kOps GCP
cluster. It is a pass/fail correctness gate, not a performance benchmark:
the run fails loudly when the warm-pool reconciler regresses on either
invariant below.

It needs a real multi-node cluster because both invariants are shapes an
envtest/fake-client setup cannot reproduce faithfully: the over-creation
race lives in the latency between a controller's create call and its own
informer observing the result (real apiserver + watch round-trips), and the
unschedulable hold depends on the real scheduler marking pods
`PodScheduled=False/Unschedulable`.

## What it proves

Both invariants come from the warm-pool over-creation issue (issue 1215,
fixed by the expectations-gated reconciler in PR 1266):

1. **No over-creation** (`warmpool-overcreate` phase): creating 20 pools x
   25 replicas in one burst produces **exactly 500 distinct Sandbox
   creates** — no more. Legitimate replacements (a create observed only
   after a member's delete, e.g. the stuck-sandbox GC) are counted and
   reported separately and tolerated up to `--wp-replacement-tolerance`
   (default 2). The concurrent live population also never exceeds the
   target: per pool, peak members <= 25 at all times. The pre-fix
   controller computed its create count from a stale informer cache and
   over-created ~10x under load (>= 5,000 creates for a 500 target,
   log-confirmed on the issue), transiently flooding the cluster far above
   the requested population.

2. **Hold, don't churn, and say so once** (`warmpool-unschedulable` phase):
   one pool of 3 replicas whose template requests `cpu: "1000"` (no machine
   shape any cloud sells comes anywhere near 1000 cores, so the pods are
   robustly unschedulable everywhere) is observed untouched for 8 minutes.
   The controller must **hold** the unschedulable sandboxes — UIDs stay
   exactly stable, zero deletes, zero extra creates — and must emit
   **exactly one** `WarmPoolNotProgressing` Warning event on the pool, with
   no duplicates. The event lands between ~5m and ~7m35s after pool
   creation (the fixed 5-minute readiness grace plus the jittered
   self-scheduled requeue). The pre-fix controller instead deleted and
   recreated the "stuck" sandboxes forever — an unbounded churn loop whose
   replacements were equally unschedulable — and emitted no signal at all.

The phases are implemented in `test/stress/warmpool.go` (flags:
`--wp-pools`, `--wp-replicas`, `--wp-image`, `--wp-replacement-tolerance`,
`--wp-unsched-replicas`, `--wp-unsched-cpu`, `--wp-unsched-watch`).

## Cluster shape and adversarial settings

The wrapper reuses the tuned `benchmarks-kops-gcp/run` cluster bring-up
(kOps on GCE: pd-ssd root volumes, KCM/scheduler client QPS at 500, etcd
metrics listeners, node-exporter — see that script for each knob's
rationale) with:

| Setting | Value | Why |
| --- | --- | --- |
| Worker nodes | 12 x `n2-standard-8` (`STRESS_NODE_COUNT`) | ~1200 pod slots: headroom for the 500-sandbox target without a long fill |
| Control plane | 1 x `c3-standard-22` (base scenario default) | keeps the control plane out of the way of the invariant measurement |
| Controller | `--sandbox-warm-pool-concurrent-workers=1000` (`CONTROLLER_ARGS`) | the adversarial worker count the over-creation reproduced under; widens the create/observe race the expectations gate must close |
| Template image | the standard small test image (the stress tool's `--image` default) | measured to trip pre-fix asserts HARDER than a multi-GB image at this shape (6.1x the create target vs 3.1x — see the FAIL section): fast readiness transitions drive more status-update reconciles/sec against the stale cache, and the quick create->Ready->excess-delete cycle adds delete churn a slow pull never reaches. Also keeps nodes free of a multi-GB image; `--wp-image` (via `STRESS_EXTRA_ARGS`) remains the escape hatch for slow-pull shapes |
| Phases | `warmpool-overcreate,warmpool-unschedulable` (`STRESS_PHASES`) | over-creation gate first, then the quiet unschedulable window |

The invariants are node-shape-independent — pass/fail depends only on
controller behavior, not node throughput (the live validation ran on
`e2-standard-8` workers with identical verdicts).

## Running it

Like the other kOps scenarios, this needs GCP credentials, a project with
quota for the cluster above, and docker for the image push:

```sh
gcloud auth login && gcloud config set project YOUR_PROJECT_ID
test/benchmarks/scenarios/benchmarks-kops-gcp-warmpool/run
```

The script creates the cluster, deploys the just-built controller with the
extensions enabled, runs the two phases, writes the usual stress artifacts
(`summary.json` carries the verdicts under `warmPoolOvercreate` /
`warmPoolUnschedulable`), and tears the cluster down.

### Expected PASS output (current main)

The run exits 0 and the stress log contains both verdict lines:

```text
[warmpool-overcreate#1] PASS: exactly 500 creates for 20 pools x 25 replicas (0 tolerated replacements), population never exceeded target
[warmpool-unschedulable#2] PASS: 3 unschedulable sandboxes held with stable UIDs, exactly one WarmPoolNotProgressing event at +<300-455>s
```

with a final report shaped like:

```text
--- #1 warmpool-overcreate: 500 requested, ... ---
  pools x replicas (target):       20 x 25 = 500
  distinct creates (POST-equiv):   500 (want 500 + replacements)
  replacements / over-creates:     0 / 0 (want over-creates 0)
  peak live per pool / global:     25 (cap 25) / 500 (cap 500)
  time until ALL pools Ready:      <fill time>s

--- #2 warmpool-unschedulable: 3 requested, ... ---
  replicas (cpu request 1000):     3, watched 480s
  distinct creates / deletes:      3 (want 3) / 0 (want 0), UIDs stable: true
  NotProgressing Warning events:   1 (want exactly 1), first at +<300-455>s
```

### Expected FAIL shape (pre-fix controllers)

The stress harness aborts after the first failed phase, and against a
controller without the expectations gate (anything before the issue-1215
fix) that is the overcreate phase — so the **full-run FAIL output is the
overcreate failure only**, exit non-zero with per-pool forensics:

```text
warmpool-overcreate#1 phase: warm pool invariants violated: N sandbox creates
beyond target without a preceding delete (over-creation); T distinct creates,
want exactly 500 + replacements; R replacements exceed tolerance 2; peak
concurrent population exceeded target in M pool(s): max P > 25 replicas
(worst pools: p1-wp-pool-...(creates=... over=... peak=...), ...)
```

Live pre-fix measurements at this exact shape (20x25, workers=1000):

* **small image (the scenario default): 3,070 distinct creates for the 500
  target (6.1x)** — 1,312 over-creates, 1,258 replacements, worst pool peak
  117 vs 25, global peak 1,796 vs 500, 2,570 deletes during the fill;
  failing leg wall time ~7 minutes.
* multi-GB not-pre-pulled image (supplementary, via `--wp-image`): 1,535
  distinct creates (3.1x), every pool over its cap, worst peak 75 vs 25.
* The original issue logs showed up to ~10x at larger scale.

PASS-direction evidence with the same small image: the fixed controller
produced exactly-600-for-600 in the single-pool gate-cost run, alongside
the two-direction 20x25 validation (PASS leg on the fixed controller, FAIL
legs above).

The unschedulable phase's failure signature
on pre-fix controllers — delete/recreate churn, unstable member UIDs, and
zero `WarmPoolNotProgressing` events within the window — never gets a
chance to print in a full run; exercise it standalone with
`STRESS_PHASES=warmpool-unschedulable`.

## Cost / duration

One ephemeral 12-node cluster (12 x n2-standard-8 + 1 x c3-standard-22,
pd-ssd roots) for **~35-45 minutes** end to end: ~10 min cluster bring-up,
a few minutes of image push/deploy and the e2e benchmark suite the base
wrapper runs, a fast overcreate fill (small image; the failing leg on a
pre-fix controller completes in ~7 min) plus settle and drain, a fixed
8-minute unschedulable window, and teardown.

## Why optional / manual

The scenario needs a real GCP cluster (project quota, ~45 min of 13 VMs),
so it is not part of the default presubmit set. Run it on demand — via
`dev/ci/presubmits/benchmarks-kops-gcp-warmpool` — when touching the
SandboxWarmPool reconciler's create/delete paths, the expectations tracker,
or the readiness-grace/unschedulable handling. The unit and envtest suites
cover the same logic deterministically on every PR; this scenario is the
live, whole-system proof.
