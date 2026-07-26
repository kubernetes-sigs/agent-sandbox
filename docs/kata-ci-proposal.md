# Runtime-Aware CI for agent-sandbox

## Motivation

agent-sandbox is designed to be runtime-agnostic: `SandboxTemplate` takes an
arbitrary `runtimeClassName`, and the controller manages warm pools, claims,
and lifecycle identically regardless of the underlying runtime. The project
ships examples for three isolation tiers:

| Tier | Runtime | Isolation | Example |
|---|---|---|---|
| Zero overhead | `runc` (default) | Linux namespaces + cgroups | `hello-world-sandbox/` |
| Process-level | `gVisor` (`runsc`) | User-space kernel intercept | `openclaw-gvisor-sandbox/` |
| Hardware-level | `kata` (`kata-qemu`, `kata-fc`) | VM per pod | `firecracker-sandbox/`, `kata-gke-sandbox/` |

**CI tests only the first tier.** Every presubmit and periodic job uses
the default `runc` runtime. The controller has no runtime-specific code
paths — if a change breaks RuntimeClass propagation, it breaks for all
runtimes, and a runc-only job catches it. **Runtime-specific blocking bugs
are unlikely by design.**

The gap is **regression visibility on timing-sensitive changes.** The
controller already runs periodic benchmarks (`benchmarks-kops-gcp-claims`)
tracking adoption latency on runc. But runc cold starts take ~0.3s — fast
enough that reconciler refactors, timeout adjustments, and `slowStartBatch`
tuning appear harmless. The same changes can disproportionately affect
runtimes where cold starts take 30-40× longer:

- A tightened readiness deadline that passes at 0.3s may fail at 12s.
- A `slowStartBatch` change that looks neutral on runc may cause resource
  exhaustion when each wave takes minutes to settle with kata.
- A reconciler optimization that reduces runc cycles from 2 to 1 may
  accidentally break the 20-30 cycle path that only kata exercises.

Without a non-runc signal, these regressions are invisible. The author
sees green CI and merges. A user deploying kata discovers the problem
weeks later.

**The value is accountability:** a red periodic job after a controller
change forces the author to explain whether the timing regression is
acceptable. It turns "works on runc" into "works across the isolation
spectrum the project claims to support."

## Current CI Landscape

| Job | Infra | Runtime | KVM? |
|---|---|---|---|
| `test-e2e` (presubmit) | KIND on EKS | runc | No |
| `benchmarks-kops-gcp-*` (presubmit + periodic) | kOps on GCE¹ (n2-standard-8 workers) | runc | No² |
| `test-load-test` (periodic) | KIND on EKS | runc | No |
| Cloud Build (images) | GCB | n/a | No |

¹ GCE = raw VMs via kOps, not GKE. kOps manages its own Kubernetes
  directly on Compute Engine instances, giving full control over instance
  configuration.

² GCE n2/n2d instances support nested virtualization natively — it is a
  per-instance metadata flag, not a hardware change. Currently not enabled.

## What This Proposal Does NOT Claim

- **No runtime-specific bugs to catch.** The controller is genuinely
  runtime-agnostic. A functional test failure on gVisor or kata that
  passes on runc would indicate a Kubernetes or runtime bug, not an
  agent-sandbox bug.
- **No production-grade performance baselines.** Nested virtualization adds
  overhead; KIND is not a production cluster. Numbers are directional, not
  absolute.
- **No new requirements on contributors.** These are periodic/optional
  jobs. A timing regression on kata does not block merge — it starts a
  conversation.

## Proposal: Timing Diversity Through Runtime Expansion

Add periodic CI jobs on runtimes with progressively heavier cold starts.
The goal is not to test runtimes — it is to exercise the controller under
timing profiles that runc cannot produce.

Each step is independently valuable and can be adopted without the others.

### Step 1 — gVisor on KIND (mild timing diversity)

**No infrastructure changes required.** The project already has a working
`kind-config.yaml` with gVisor containerd patches
(`examples/openclaw-gvisor-sandbox/kind-config.yaml`).

**What changes:**

1. Add a `test-e2e-gvisor` presubmit job in `dev/ci/presubmits/`
2. The job creates a KIND cluster with `--config kind-config.yaml` that
   registers the `runsc` runtime with containerd
3. Apply a `gvisor` RuntimeClass (`handler: runsc`)
4. Set `SANDBOX_RUNTIME_CLASS=gvisor` to activate RuntimeClass-aware tests
5. Run the functional test suite (`--suite tests`)

**What it adds:**

- A second data point on the cold-start spectrum (~0.5s vs runc's ~0.3s)
- Validates that RuntimeClass propagation works end-to-end with an actual
  non-default runtime (not just the field being set)
- Exercises pool fill/drain under slightly slower pod readiness
- Near-zero cost — same KIND infrastructure, same CI budget

**Limitations:**

- gVisor's overhead is small relative to kata — it won't surface timing
  bugs that need a 10s+ cold start to trigger
- Not a VM, so no KVM/hardware interaction coverage

**Effort:** Minimal. Copy the existing `test-e2e` job, add the
`kind-config.yaml` containerd patch and RuntimeClass, set one env var.

### Step 2 — kata on kOps (extreme timing diversity)

Enable nested virtualization on the existing kOps GCE benchmark
infrastructure. This is where the real regression signal lives — kata
cold starts are 30-40× slower than runc, amplifying every timing
assumption the controller makes.

**What changes:**

1. Add a `benchmarks-kops-gcp-kata` periodic job in `dev/ci/periodics/`
2. Modify the kOps cluster creation to add instance metadata
   `enable-nested-virtualization=true` on n2 workers
3. Add a `kata-deploy` DaemonSet step after cluster creation to install kata
   binaries and register the runtime with containerd on worker nodes
4. Set `SANDBOX_RUNTIME_CLASS=kata-qemu` to activate RuntimeClass-aware tests
5. Run as **periodic** (daily or weekly) — not presubmit, since kata VM
   startup adds 8-12s per sandbox

**What it adds:**

- **Timeout regression detection** — a readiness deadline that works at
  0.3s but fails at 12s shows up here, not in production
- **`slowStartBatch` stress** — with kata the controller creates wave
  after wave while earlier pods are still booting. Queue depth, memory
  pressure, and expectations tracker overflow are exercised under
  conditions runc never produces
- **Reconciliation storm coverage** — runc pods go Ready in 1-2
  reconcile cycles. Kata pods trigger 20-30 cycles per pod while booting.
  Edge cases (duplicate creates, stale informer cache) get amplified
- **Trend data** — periodic runs build a time series. A PR that doubles
  kata fill time from 2min to 4min is visible in the history even if the
  job stays green

**VMM family expansion:** `kata-deploy` installs multiple VMMs —
`kata-qemu`, `kata-clh` (Cloud Hypervisor), and `kata-fc` (Firecracker).
Once the nested virt infrastructure is in place, adding a job for a
different VMM is a single env var change
(`SANDBOX_RUNTIME_CLASS=kata-fc`). The VMM family spans a wide range of
boot time (~125ms for Firecracker vs ~8-12s for QEMU) and RAM footprint,
providing additional data points on the timing spectrum without any
infrastructure cost.

**Limitations:**

- Nested virtualization adds ~10-15% overhead vs bare metal KVM — numbers
  are directional, not production-representative
- No confidential computing / attestation coverage

**Effort:** Low — configuration changes to existing kOps scripts plus a new
Prow job definition. No new infrastructure or budget required. Uses
existing Boskos GCE project pool.

**Cost:** Same instance pricing as current benchmark jobs. Nested
virtualization does not change GCE pricing.

### Optional — Bare-metal kata (future)

Donate physical servers with VT-x/AMD-V for production-grade kata
benchmarks without nested virtualization overhead. This adds true cold start
baselines and realistic density testing.

**When to consider:** After Steps 1-2 prove the regression signal is
useful and someone is actually looking at the numbers. Only if the
community needs production-representative performance data for release
gating.

**Effort:** High — hardware procurement, CNCF donation process, ongoing
maintenance.

## Test Harness

The RuntimeClass-aware test harness is introduced in
[PR #1262](https://github.com/kubernetes-sigs/agent-sandbox/pull/1262).
All tests activate with a single env var (`SANDBOX_RUNTIME_CLASS`) and
auto-adapt pool sizing to cluster capacity.

### Functional tests (all runtimes, CI-suitable)

| Test | What it validates |
|---|---|
| `TestRuntimeClassLifecycle` | Pool fill → claim → refill → claim cycle |
| `TestRuntimeClassStartupComparison` | Cold vs warm speedup ratio |
| `TestSandboxClaimAdoption` | Claim adopts from warm pool, RuntimeClass propagates |

### Load and benchmark tests (periodic, runtime-scaled)

| Test | What it validates |
|---|---|
| `TestRuntimeClassBurstRecovery` | Sustained batch load with per-claim milestone decomposition (ACK, adoption, schedule, runtime, propagation) |
| `BenchmarkRuntimeClassColdStart` | Cold start latency distribution |
| `BenchmarkRuntimeClassWarmClaim` | Warm claim latency distribution |

Key behaviors:

- **`isVMRuntime()`** automatically caps pool sizes by CPU capacity for
  kata (1 VM = 1 CPU), prevents over-provisioning
- **`benchPoolSizes()`** defaults to `{half, full, 2×}` of available CPU
  capacity when `SANDBOX_POOL_SIZES` is not set
- **Multi-size pool sweep** tests pool behavior across small → large
  configurations on each runtime
- **CSV milestone output** with cluster metadata (instance type, runtime,
  pool size) for cross-runtime comparison
- **Configurable burst parameters** via `SANDBOX_BATCH_CAP` and
  `SANDBOX_SETTLE_SEC` for tuning per-runtime sensitivity

### Why timing diversity matters

The same test code, running across all three runtimes, produces
dramatically different controller behavior:

```text
runc (0.3s cold)  ──→  gVisor (0.5s cold)  ──→  kata (8-12s cold)
    │                        │                        │
    ├─ pool fills in <2s     ├─ pool fills in ~3s      ├─ pool fills in minutes
    ├─ 1-2 reconcile cycles  ├─ 2-3 reconcile cycles   ├─ 20-30 cycles per pod
    ├─ warm claims ~300ms    ├─ warm claims ~400ms     ├─ warm claims ~300ms
    └─ timing bugs hidden    └─ mild amplification     └─ timing bugs exposed
```

Warm claims are runtime-agnostic (pod already exists). Cold starts and
pool refill timing scale dramatically — this is where controller changes
that look harmless on runc reveal their real cost.

## Proposed Job Configurations

### Step 1 — gVisor presubmit

```yaml
name: pull-agent-sandbox-e2e-gvisor
cluster: eks-prow-build-cluster
always_run: false
run_if_changed: '^(extensions/|test/e2e/|examples/.*gvisor)'
decorate: true
spec:
  containers:
  - image: gcr.io/k8s-staging-test-infra/kubekins-e2e:latest
    command: [dev/ci/presubmits/test-e2e]
    args: ["--suite", "tests"]
    env:
    - name: SANDBOX_RUNTIME_CLASS
      value: gvisor
    - name: KIND_CONFIG
      value: examples/openclaw-gvisor-sandbox/kind-config.yaml
```

### Step 2 — kata periodic

```yaml
name: ci-agent-sandbox-kata-e2e-periodic
cluster: k8s-infra-prow-build
interval: 24h
decorate: true
spec:
  containers:
  - image: gcr.io/k8s-staging-test-infra/kubekins-e2e:latest
    command: [runner.py]
    args:
    - --scenario=benchmarks-kops-gcp
    env:
    - name: SANDBOX_RUNTIME_CLASS
      value: kata-qemu
    - name: SANDBOX_POOL_SIZES
      value: "4,6,8,12,16"
    - name: SANDBOX_BATCH_CAP
      value: "10"
    - name: SANDBOX_SETTLE_SEC
      value: "2"
    - name: KOPS_NESTED_VIRT
      value: "true"
```

*Note: exact fields depend on the job generator templates and runner
scripts. The above is illustrative.*

## Implementation Roadmap

### Phase 1 — gVisor on KIND (Step 1)

1. Verify `examples/openclaw-gvisor-sandbox/kind-config.yaml` works with
   the e2e test runner (may need `runsc` pre-installed on the CI node
   image, or a download step)
2. Add `gvisor` RuntimeClass creation to the test setup
3. Add `test-e2e-gvisor` presubmit to `dev/ci/presubmits/`
4. PR to `kubernetes/test-infra` to register the new Prow job

### Phase 2 — kata on kOps (Step 2)

1. Modify kOps cluster creation script to enable nested virtualization
   on n2 worker instances
2. Add `kata-deploy` DaemonSet step to install kata binaries post-cluster
3. Add `benchmarks-kops-gcp-kata` periodic job
4. PR to `kubernetes/test-infra` to register the new Prow job
5. Validate with `pj-on-kind.sh` (see `docs/prowjob_manual_run.md`)

### Phase 3 — Multi-runtime dashboard (optional)

Publish CSV benchmark outputs to a shared storage bucket. Build a simple
dashboard that overlays claim latency distributions across runtimes and
pool sizes for trend detection.

## Dependencies

- [PR #1262](https://github.com/kubernetes-sigs/agent-sandbox/pull/1262) —
  RuntimeClass-aware test harness and benchmark suite. Steps 1 and 2
  depend on this PR merging.

## References

- [gVisor KIND example](../examples/openclaw-gvisor-sandbox/) — working
  `kind-config.yaml` and RuntimeClass setup
- [Firecracker sandbox example](../examples/firecracker-sandbox/) — kata-fc
  prerequisites and KVM requirements
- [Kata GKE sandbox example](../examples/kata-gke-sandbox/) — kata-qemu on GKE
- [GCE nested virtualization](https://cloud.google.com/compute/docs/instances/nested-virtualization/managing-constraint)
- [kata-deploy](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy) —
  upstream DaemonSet for kata installation on containerd clusters
- [agent-sandbox Prow jobs](https://github.com/kubernetes/test-infra/tree/master/config/jobs/kubernetes-sigs/agent-sandbox)
- [Runtime class test README](../test/e2e/extensions/) — env var reference
  and benchmark configuration
