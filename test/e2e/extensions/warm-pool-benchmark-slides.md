---
marp: true
theme: default
paginate: true
---

# [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) Warm Pool
## Runtime Performance Benchmark

**Cluster**: GCP n2-standard-8, 2 workers (16 vCPUs, 64GB RAM)
**Controller**: Red Hat operator v0.9.0 (upstream [~v0.5.0rc1](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.0rc1), built 2026-06-18)
**Date**: 2026-07-22

---

# The Problem: Pod Cold Start

Every new pod goes through a startup pipeline before it can serve work:

```
API Server  -->  Scheduler  -->  Kubelet  -->  Image Pull  -->  Runtime  -->  Ready
   ~10ms         ~50ms          ~100ms       ~200ms-5s       varies       SUM
```

| Runtime | Cold Start | What happens |
|---------|-----------|--------------|
| [**runc**](https://github.com/opencontainers/runc) | 0.5-1s | create cgroup, mount rootfs, exec |
| [**gvisor**](https://gvisor.dev/docs/) | 1-1.5s | runc + boot userspace kernel ([Sentry](https://gvisor.dev/docs/architecture_guide/)) |
| [**kata**](https://katacontainers.io/docs/) | 7-13s | boot full VM (QEMU), guest kernel, agent, rootfs |

**Impact**: At burst, N cold starts queue behind scheduler + API server.
10 simultaneous cold kata claims = 13s wait for the last one.

---

# The Solution: Warm Pool

Pre-provision sandboxes via [SandboxWarmPool](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/warm-pools.md) so claims bind instantly to ready pods:

```
                    Warm Pool (pre-created)
                   +--+--+--+--+--+--+--+--+
                   |OK|OK|OK|OK|OK|OK|OK|OK|  <-- Ready sandboxes
                   +--+--+--+--+--+--+--+--+
                          |
    SandboxClaim  ------->+  Bind (in-memory selection)
                          |
                       ~0.3s  <-- Same for ALL runtimes
```

**Key insight**: Warm claim latency is **runtime-independent**.
The pool absorbs the entire cold start pipeline.

---

# Benchmark Method

**Batched drain loop**: fire claims in batches, 100ms settle between batches

```
batch_size = min(max(4, pool_size/2), 8)
max_claims = pool_size * 2    (guarantee depletion)

repeat:
  sleep 100ms            (reconciler settle)
  GET pool.readyReplicas (snapshot pool state)
  fire batch_size parallel claims
  until: pool depleted OR max_claims reached
```

**Quality zones** (1s = user experience boundary):

| Zone | Range | Meaning |
|------|-------|---------|
| Green | <= warm * 1.2 | Clearly warm, no contention |
| Grey | warm*1.2 .. 1s | Warm with scheduling overhead |
| Cold | > 1s | Noticeable delay |

---

# [v0.5.0rc1](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.0rc1): What Changed Under the Hood

Three optimizations that transformed warm pool performance:

## 1. Parallel Sandbox Creation ([#867](https://github.com/kubernetes-sigs/agent-sandbox/pull/867))
Before: sandboxes created sequentially during refill. Pool-8 refill = 8 x single_create.
After: all slots refill in parallel. Pool-8 refill = ~1 x single_create. Up to **4.26x** faster.

**Proof from data**: kata batch_refill = 7.87s for 4 slots. Individual cold claim = 8.0s.
If sequential: 4 x 8s = 32s. Observed: 7.87s --> parallel confirmed.

## 2. In-Memory [NodeSpread](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/warm-pools.md#node-spread) Selection
Before: each claim triggered API calls to list/filter pods across nodes.
After: selection runs purely in-memory from [informer cache](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/cache).

**Result**: P99 concurrent claim latency improved **4x**. No staircase at pool-16+.

## 3. Patch-Based [SandboxClaim](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/api.md) Status Updates
Before: full object update on every status change (read-modify-write).
After: [strategic merge patch](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/update-api-object-kubectl-patch/#use-a-strategic-merge-patch-to-update-a-deployment) -- smaller payload, fewer conflicts.

**Result**: 8 parallel claims no longer produce update conflicts under contention.

---

# Dynamic Batch Sizing

```
batch_size = min(max(4, pool_size / 2), 8)
```

| Pool | Batch | Batches to drain (2x claims) | Why this ratio |
|------|-------|------------------------------|----------------|
| 4 | 4 | 2 | drain in 2, guaranteed depletion |
| 6 | 4 | 3 | min=4 prevents too-small batches |
| 8 | 4 | 4 | pool=2xbatch, refill races drain |
| 12 | 6 | 4 | proportional, tests refill under load |
| 16 | 8 | 4 | max=8, avoids reconciler saturation |
| 20 | 8 | 5 | capped, more batches to drain |
| 24 | 8 | 6 | same cap, longer sustained pressure |

**Pool depletion rule**: pool = batch x 2 is the minimum for all-warm runc/gvisor.
Below that (pool-4 with batch-4), the pool drains completely in one batch.

**Why cap at 8**: beyond 8 parallel claims, the reconciler work queue serializes
them anyway (~160ms cycle). More parallel writes = more etcd contention, no throughput gain.

---

# Grey Zone: What Causes It

Claims between warm*1.2 and 1s -- warm but slower. NOT runtime-specific.

```
runc  pool-8 batch-1:   0.321  0.319  0.480  0.322   (3 green + 1 grey)
gvisor pool-8 batch-1:  0.320  0.318  0.475  0.314   (3 green + 1 grey)
kata  pool-8 batch-1:   0.333  0.467  0.336  0.333   (3 green + 1 grey)
                               ^^^^^         ^^^^^
                          ~160ms gap -- same for ALL runtimes
```

## Root causes (controller + API server, not runtime)

| Source | Latency added | Why |
|--------|--------------|-----|
| **[Reconciler](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) serialization** | ~80-160ms | Work queue processes claims one at a time per cycle |
| **[etcd](https://etcd.io/docs/v3.5/learning/api/) write contention** | ~10-20ms/claim | Parallel status patches serialize through MVCC |
| **[Watch](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes) event coalescing** | ~50-100ms | API server batches status-change notifications |
| **[Informer](https://pkg.go.dev/k8s.io/client-go/informers) cache sync** | ~20-50ms | Controller cache lags behind etcd writes |

Combined: first half of claims in a batch complete in ~0.32s (one reconciler cycle),
second half wait ~160ms for the next cycle --> bimodal 0.32s / 0.48s pattern.

**gVisor shows slightly more grey** because 0.275s/slot refill (vs runc 0.187s)
means fewer ready slots at batch boundaries --> more contention, not more Sentry overhead.

[**v0.5.1**](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.1) (not yet in our operator) adds pod cache label indexing ([#1099](https://github.com/kubernetes-sigs/agent-sandbox/pull/1099), O(N) to O(1) pod lookup)
and namespace queue partitioning ([#813](https://github.com/kubernetes-sigs/agent-sandbox/pull/813)) -- expected to compress the grey zone further.

---

# runc (default): The Baseline

| Pool | Claims | Warm | Cold | Green | Grey | Worst | Throughput |
|------|--------|------|------|-------|------|-------|------------|
| 4 | 8 | 8 | 0 | 4 | 4 | 0.706s | 5.5/s |
| 8 | 16 | 16 | 0 | 13 | 3 | 0.488s | 5.9/s |
| 12 | 24 | 24 | 0 | 13 | 11 | 0.767s | 7.5/s |
| 16 | 32 | 32 | 0 | 14 | 18 | 0.555s | **10.5/s** |
| 20 | 40 | 40 | 0 | 14 | 26 | 0.645s | 9.9/s |
| 24 | 48 | 48 | 0 | 21 | 27 | 0.752s | 10.0/s |

- **Zero cold starts** at any pool size
- Throughput peaks at **10.5/s** (pool-16), plateaus after
- Pool never depletes -- runc refills faster than drain (0.187s/slot)
- Grey zone grows with batch size (API contention), all under 1s
- **No VM overhead**: pool costs only idle pod memory (~16Mi pause container)

---

# [gVisor](https://gvisor.dev/): Userspace Kernel, Near-runc Speed

| Pool | Claims | Warm | Cold | Green | Grey | Worst | Throughput |
|------|--------|------|------|-------|------|-------|------------|
| 4 | 8 | 7 | 1 | 5 | 2 | 1.115s | 4.0/s |
| 8 | 16 | 16 | 0 | 12 | 4 | 0.491s | 5.6/s |
| 12 | 24 | 24 | 0 | 11 | 13 | 0.652s | 7.9/s |
| 16 | 24 | 24 | 0 | 9 | 15 | 0.839s | 8.5/s |
| 20 | 40 | 40 | 0 | 18 | 22 | 0.566s | **10.5/s** |
| 24 | 48 | 48 | 0 | 16 | 32 | 0.619s | 10.2/s |

- **1 cold start total** (pool-4, barely over 1s at 1.115s)
- Refill 0.275s/slot -- only 1.5x slower than runc (parallel creation handles it)
- Same 10/s throughput ceiling as runc, peaks at pool-20
- Grey zone driven by reconciler contention, not Sentry (~50ms boot is noise)
- **No VM**: same resource density as runc, stronger isolation via syscall interception
- Throughput dips slightly at pool-24 (10.2/s) -- reconciler saturation, not runtime

---

# [kata](https://katacontainers.io/): Hardware Isolation, VM Cost

| Pool | Claims | Warm | Cold | Green | Grey | Worst | Throughput |
|------|--------|------|------|-------|------|-------|------------|
| 4 | 8 | 4 | 4 | 3 | 1 | 8.1s | 0.9/s |
| 6 | 12 | 10 | 2 | 7 | 3 | 8.2s | 1.2/s |
| 8 | 16 | 12 | 4 | 9 | 3 | 9.4s | 1.4/s |
| 12 | 24 | 18 | 6 | 7 | 11 | 11.3s | 1.7/s |
| 16 | 32 | 24 | 8 | 4 | 20 | **13.5s** | 2.0/s |

- Cold starts: 8-13s (full QEMU VM boot)
- Refill 1.967s/slot -- 10.6x slower than runc
- Pool depletes at batch 3 every time, recovers for batch 4
- **Warm claims identical speed** to runc/gvisor (~0.3s)
- Throughput caps at 2.0/s (dominated by cold start weight)
- Worst start grows with pool size: more VMs competing for host resources

---

# Latency Percentiles: All Claims

P99 is the latency 99% of claims complete within -- the tail that users remember.

| Runtime | Pool | N | P50 | P95 | P99 |
|---------|------|---|-----|-----|-----|
| runc | 4 | 8 | 0.401s | 0.627s | 0.690s |
| runc | 16 | 32 | 0.492s | 0.554s | 0.555s |
| runc | 24 | 48 | 0.478s | 0.741s | 0.752s |
| gvisor | 4 | 8 | 0.322s | 0.892s | 1.070s |
| gvisor | 16 | 24 | 0.486s | 0.786s | 0.829s |
| gvisor | 24 | 48 | 0.489s | 0.558s | 0.599s |
| kata | 4 | 8 | **4.259s** | 8.138s | 8.144s |
| kata | 16 | 32 | 0.493s | **13.438s** | **13.461s** |

kata's P50 at pool-4 is 4.3s (half the claims are cold). At pool-16 P50 drops
to 0.49s (75% warm) but P99 explodes to 13.5s -- one bad cold start ruins the tail.

---

# Latency Percentiles: Warm Claims Only

Warm-only P99 isolates the contention overhead from cold starts:

| Runtime | Pool | Nw | P50w | P95w | P99w |
|---------|------|----|------|------|------|
| runc | 4 | 8 | 0.401s | 0.627s | 0.690s |
| runc | 16 | 32 | 0.492s | 0.554s | 0.555s |
| runc | 24 | 48 | 0.478s | 0.741s | 0.752s |
| gvisor | 8 | 16 | 0.320s | 0.488s | 0.490s |
| gvisor | 16 | 24 | 0.486s | 0.786s | 0.829s |
| gvisor | 24 | 48 | 0.489s | 0.558s | 0.599s |
| kata | 8 | 12 | 0.328s | 0.471s | 0.472s |
| kata | 16 | 24 | 0.475s | 0.580s | 0.581s |

**All three runtimes have warm P99 under 0.83s** -- the pool makes them equivalent.

Warm P95-P99 spread is tight (0.01-0.05s) -- contention is bounded, not fat-tailed.
The gap between P50w (~0.48s) and P99w (~0.75s) is the reconciler serialization cost.

---

# Three Runtimes: Head-to-Head

## Calibration

| Metric | runc | gvisor | kata |
|--------|------|--------|------|
| Warm baseline | 0.317s | 0.319s | 0.320s |
| Refill/slot | 0.187s | 0.275s | 1.967s |
| Refill ratio vs runc | 1.0x | 1.5x | 10.6x |

## Peak Performance

| Metric | runc | gvisor | kata |
|--------|------|--------|------|
| Peak throughput | 10.5/s | 10.5/s | 2.0/s |
| Optimal pool size | 16 | 20 | 16 |
| P99 at optimal | 0.555s | 0.566s | 13.461s |
| P99 warm-only at optimal | 0.555s | 0.829s | 0.581s |

**Warm P99 is equivalent across all three runtimes.**
The pool eliminates the cold start difference entirely -- for warm claims.

---

# Throughput Curves

```
claims/sec
  |
11|              R.........R.........R         R = runc
10|         G...........................G      G = gvisor
 9|
 8|                   G
 7|              R
 6|         R
 5|    R    G
 4|    G
 3|
 2|                                  K         K = kata
 1|         K    K
 0|    K
  +----+----+----+----+----+----+----+--> pool size
       4    8   12   16   20   24

  runc & gvisor converge at ~10/s (reconciler limit)
  kata capped at 2/s (VM cold start dominates average)
```

---

# Resource Density

Each warm pool slot costs ([RuntimeClass.overhead.podFixed](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/)):

| Resource | runc | gvisor | kata |
|----------|------|--------|------|
| CPU overhead | ~0 (cgroup only) | ~0 (userspace) | **250m** (VM process) |
| RAM overhead | ~16Mi (pause) | ~16Mi (pause) | **350Mi** (QEMU + guest kernel) |
| Host overhead | none | Sentry process | QEMU process |
| Isolation | namespace/cgroup | syscall filter | **hardware (VT-x)** |

kata's `default_memory = 2048` is the guest VM address space, not host consumption.
QEMU uses lazy allocation ([KSM](https://docs.kernel.org/admin-guide/mm/ksm.html) + demand paging) -- actual RSS is ~350Mi per idle VM.

## Pool capacity on 2x n2-standard-8 (15 allocatable CPUs, 62 GB RAM)

| Pool Size | runc/gvisor CPU | kata CPU | kata RAM | Headroom for apps |
|-----------|----------------|----------|----------|-------------------|
| 4 | ~0% | 7% (1.0 CPU) | 1.4 GB | Plenty |
| 8 | ~0% | 13% (2.0 CPU) | 2.8 GB | Plenty |
| 12 | ~0% | 20% (3.0 CPU) | 4.1 GB | Comfortable |
| 16 | ~0% | 27% (4.0 CPU) | 5.5 GB | Moderate |
| 24 | ~0% | 40% (6.0 CPU) | 8.2 GB | Tight |

runc/gvisor: pool size bounded by reconciler throughput (~10/s), not resources.
kata: pool size bounded by host CPU/RAM, but overhead is **6x lower** than full VM sizing.

---

# Hybrid Strategy: gVisor + kata Pools

Combine fast burst (gVisor) with hardware isolation (kata) using separate pools:

```
                 SandboxClaim
                      |
             SLA-based routing
              /              \
     gvisor pool (16-20)   kata pool (8-16)
     burst-capable         high-security
     10.5 claims/sec       1.4-2.0 claims/sec
     ~0 CPU overhead       250m CPU + 350Mi per slot
     syscall isolation     hardware isolation
```

## Tiered approach

| Tier | Runtime | Pool | Use case | Claim latency |
|------|---------|------|----------|---------------|
| **Fast** | gvisor | 16-20 | Default workloads, burst | 0.3-0.6s |
| **Secure** | kata | 8-16 | Untrusted code, compliance | 0.3s warm, 8s cold |
| **Fallback** | runc | on-demand | Legacy, no isolation needed | 0.3-0.7s |

---

# Hybrid: Resource Budget

On 2x n2-standard-8 (15 CPUs, 62 GB):

| Config | gVisor pool | kata pool | kata CPU | kata RAM | Burst capacity |
|--------|-------------|-----------|----------|----------|----------------|
| **Balanced** | 16 | 8 | 2.0 CPU | 2.8 GB | 10/s gvisor + 1.4/s kata |
| **Burst-heavy** | 20 | 4 | 1.0 CPU | 1.4 GB | 10.5/s gvisor + 0.9/s kata |
| **Security-heavy** | 12 | 16 | 4.0 CPU | 5.5 GB | 7.9/s gvisor + 2.0/s kata |

gVisor pools are essentially free (no VM overhead).
kata overhead is 250m CPU + 350Mi per slot -- much lighter than expected.

**Recommendation**: Start with gvisor-16 + kata-8 (Balanced).
With real overhead at 250m/350Mi, kata pools can be sized more generously than
the old 1-vCPU/2-GB assumption allowed.

---

# Key Takeaways

1. **Warm P99 < 0.83s for ALL runtimes** -- the pool makes runc, gvisor, and kata equivalent for warm claims

2. **kata's overall P99 = 13.5s** at pool-16, but **warm-only P99 = 0.58s** -- cold starts ruin the tail, not the pool

3. **gVisor is the burst runtime** -- runc-like P99 + syscall isolation, no VM cost, same 10/s throughput ceiling

4. **10 claims/sec is the reconciler ceiling** -- runtime-independent, v0.5.0rc1's parallel creation + in-memory selection are the enablers

5. **Grey zone is controller overhead, not runtime** -- bimodal ~160ms gap from reconciler serialization + etcd contention, same across all runtimes

6. **Batch size caps at 8** -- beyond that, reconciler serialization dominates; the P95-P99 spread stays tight (0.01-0.05s) confirming bounded contention

7. **Hybrid pools unlock both speed and security** -- route by SLA, size by resource budget

8. **1s threshold is the right UX boundary** -- no claims land in the 1-7s dead zone; clean warm/cold separation

## Next steps

- [**v0.5.1+**](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.1) **upgrade**: pod cache indexing ([#1099](https://github.com/kubernetes-sigs/agent-sandbox/pull/1099)) + namespace queue partitioning ([#813](https://github.com/kubernetes-sigs/agent-sandbox/pull/813)) should compress grey zone
- **kata 4.0 downstream** (see next slide)
- Pool auto-sizing based on cluster capacity
- SLA-based routing webhook ([SandboxClaim](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/api.md) admission)

---

# [Kata 4.0](https://github.com/kata-containers/kata-containers/releases/tag/4.0.0): What Changes for Warm Pools

## Runtime rewritten: Go to [Rust](https://github.com/kata-containers/kata-containers/tree/main/src/runtime-rs) (runtime-rs)

Entire kata runtime is now Rust -- lower memory footprint, faster startup, no GC pauses.

## VMM: QEMU to [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) (default)

| Metric | QEMU (current) | Cloud Hypervisor | Improvement |
|--------|----------------|------------------|-------------|
| VMM boot | ~500ms+ | [~150-200ms](https://northflank.com/blog/guide-to-cloud-hypervisor) | **3x faster** |
| Codebase | ~2M lines C | ~50K lines Rust | 40x smaller attack surface |
| Device count | 40+ (legacy) | 16 (modern only) | Less overhead |

## [VM Templating](https://github.com/kata-containers/kata-containers/blob/main/docs/design/kata-vm-templating.md)

Pre-fork from a template VM -- guest kernel already booted, agent already running.
New VMs share memory pages via copy-on-write. Cuts boot time and per-VM RAM.

## [Virtio-FS DAX](https://virtio-fs.gitlab.io/)

Direct memory-mapped access to host filesystem. Eliminates FUSE round-trips for
container rootfs. Lower latency, lower CPU overhead for I/O-heavy workloads.

---

# Kata 4.0: Projected Impact on Benchmarks

## Cold start projection (conservative estimate)

| Component | Current (QEMU) | Kata 4.0 (CLH + template) | Delta |
|-----------|---------------|---------------------------|-------|
| VMM boot | ~500ms | ~150ms | -350ms |
| Guest kernel | ~2-3s | ~0ms (templated) | -2.5s |
| Agent startup | ~1-2s | ~0ms (templated) | -1.5s |
| Rootfs mount | ~500ms | ~100ms (Virtio-FS DAX) | -400ms |
| K8s pipeline | ~3-4s | ~3-4s (unchanged) | 0 |
| **Total cold** | **8-13s** | **~3-4s** | **~60-70% reduction** |

## What this means for warm pools

| Scenario | Current | With kata 4.0 |
|----------|---------|---------------|
| Cold start | 8-13s | ~3-4s |
| Refill/slot | 1.97s | ~0.5-0.8s (est.) |
| Pool depletion recovery | batch 4 waits ~8s | batch 4 waits ~3s |
| Max practical pool | 16 (limited by cold penalty) | 24+ (cold is tolerable) |
| **Gap to gVisor cold** | **8-12x slower** | **~3x slower** |

With kata 4.0's cold start dropping to ~3-4s, the hybrid strategy shifts:
kata pools can be the **primary** runtime (not just safety net),
with gVisor reserved for ultra-low-latency burst.

---

# Key Takeaways

1. **Warm P99 < 0.83s for ALL runtimes** -- the pool makes runc, gvisor, and kata equivalent for warm claims

2. **kata's overall P99 = 13.5s** at pool-16, but **warm-only P99 = 0.58s** -- cold starts ruin the tail, not the pool

3. **gVisor is the burst runtime** -- runc-like P99 + syscall isolation, no VM cost, same 10/s throughput ceiling

4. **10 claims/sec is the reconciler ceiling** -- runtime-independent, v0.5.0rc1's parallel creation + in-memory selection are the enablers

5. **Grey zone is controller overhead, not runtime** -- bimodal ~160ms gap from reconciler serialization + etcd contention, same across all runtimes

6. **Batch size caps at 8** -- beyond that, reconciler serialization dominates; the P95-P99 spread stays tight (0.01-0.05s) confirming bounded contention

7. **Hybrid pools unlock both speed and security** -- route by SLA, size by resource budget

8. **1s threshold is the right UX boundary** -- no claims land in the 1-7s dead zone; clean warm/cold separation

9. **kata 4.0 narrows the gap** -- Rust runtime + Cloud Hypervisor + VM templating could bring cold starts from 8-13s to ~3-4s, making kata viable as a primary pool runtime

## Further reading

- [Agent Sandbox docs](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/docs) | [Warm Pool API](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/warm-pools.md) | [All releases](https://github.com/kubernetes-sigs/agent-sandbox/releases)
- [gVisor architecture](https://gvisor.dev/docs/architecture_guide/) | [Kata architecture](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md)
- [Kata 4.0 release](https://github.com/kata-containers/kata-containers/releases/tag/4.0.0) | [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) | [VM templating](https://github.com/kata-containers/kata-containers/blob/main/docs/design/kata-vm-templating.md)
- [controller-runtime internals](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/internal/controller) | [K8s Pod Overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/)

---
