# rl-tunix SWE-bench warm pools — Performance log

Working notes (local, git-ignored). Tracks measured performance of runs and the
optimizations we test. All times in seconds unless noted. See
[`optimizations.md`](./optimizations.md) for the idea backlog.

## Environment

| Item | Value |
| :--- | :--- |
| Cluster | `glottman-sandbox-test-1` (GKE Standard) |
| Project / Region | `gke-ai-eco-dev` / `us-central1` |
| GKE version (master/node) | `1.35.5-gke.1000000` |
| kubectl (client/server) | `v1.35.3-dispatcher` / `v1.35.5-gke.1000000` |
| Node pool | `standard-pool` — `e2-standard-2`, **3 nodes**, no autoscaling |
| Disk / type | 100 GB `pd-balanced` per node |
| Allocatable / node | ~`1930m` CPU, ~`5.9Gi` mem (≈ **5.8 vCPU / 17.6Gi** total) |
| Accelerators | none (no GPU/TPU) |
| Agent Sandbox controller | `…/agent-sandbox-repo/agent-sandbox-controller:latest` (custom main build) |
| CRD API version | `extensions.agents.x-k8s.io/v1beta1` (installed 2026-05-28) |
| RuntimeClasses | `gvisor`, `confidential-linked-runner` (no gVisor node pool → **gvisor unused**) |
| Sandbox-router | not deployed (exec is router-free via `kubectl exec`) |
| Task images | `slimshetty/swebench-verified:sweb.eval.x86_64.<instance>` (~1.2 GB each, Docker Hub) |

> Update this table whenever the cluster/version/pool/controller changes, and
> note the change date.

## Sandbox lifecycle & terminology

"Claimed" = a sandbox has been handed to a consumer (the model's rollout) for
**exclusive use**.

| State | Meaning | Usable by the model? |
| :--- | :--- | :--: |
| **Warm / pooled** | `SandboxWarmPool` keeps N sandboxes pre-created + Ready, but **unassigned** (idle). | No |
| **Claimed** | A `SandboxClaim` adopted one warm sandbox and **bound it to you** (`status.sandbox.name`). | **Yes** |
| **Released** | Claim deleted → sandbox **destroyed** (not returned to the pool). | No |

- **Claim = allocation.** Creating a `SandboxClaim` (with `warmPoolRef`) makes the
  controller pick a pre-warmed sandbox and dedicate it to that claim.
- **Exclusive + singleton.** One claim ↔ one sandbox ↔ one pod; never shared.
- **The pool refills.** Taking a warm sandbox triggers a replacement to keep
  `replicas` Ready (object dumps show your sandbox + a fresh one).
- **Not checkout/return.** Releasing = deleting the claim = the sandbox is torn
  down, not recycled. Each task gets a fresh one (= claim-per-task churn; see
  optimization #4 in `optimizations.md`).
- **What `clm` measures:** time-to-available = create claim → adopt warm sandbox
  → resolve name → confirm Ready. Seconds with a warm pool; includes cold-start
  without pre-warming.

## Runs

Phase columns are wall-clock seconds for that phase (aggregated across tasks).
**Cached** = task image already present on the node (no multi-GB pull).
Legend: prov=provision, warm=wait-for-ready (pull), clm=claim, exec=exec probe,
tear=teardown.

| Date (local) | Tool | Strategy | Tasks | Win/MaxPool | Opt tested | Cached | prov | warm | clm | exec | tear | **TOTAL** | Notes |
| :--- | :--- | :--- | :--: | :--: | :--- | :--: | --: | --: | --: | --: | --: | --: | :--- |
| 2026-06-18 | e2e_test.sh | naive | 1 | –/1 | baseline | yes | 3.8 | 1.0 | 3.8 | 2.6 | 5.0 | **28.5** | astropy-12907 |
| 2026-06-18 | e2e_test.sh | none | 1 | –/1 | baseline | yes | 4.1 | 1.1 | 4.1 | 2.6 | 5.0 | **30.9** | astropy-12907 |
| 2026-06-18 | e2e_test.sh | sliding | 2 | 1/8 | baseline | yes | – | – | – | – | – | **53.8** | 12907→13033; only total captured |
| 2026-06-18 | e2e_test.sh | naive | 1 | –/1 | cold, same family | partial | 3.8 | **11.0** | 3.9 | 2.7 | 5.0 | **38.3** | astropy-13236; base layers already cached (astropy) |
| 2026-06-18 | e2e_test.sh | naive | 1 | –/1 | **cold, fresh family** | no | 3.8 | **80.9** | 3.9 | 2.7 | 4.9 | **108.2** | django-10097; true cold pull (~1.2 GB) |
| 2026-06-18 | e2e_test.sh | naive | 1 | –/1 | **pre-pull (#1)** | pre-pulled | 3.7 | **6.0** | 3.7 | 2.6 | 4.9 | **34.0** | django-11095; warm phase = pod start only, pull done by DaemonSet |
| 2026-06-19 | e2e_test.sh | none | 10 | –/1 | strategy compare | astropy (base cached) | 37.8 | 89.2 | 37.5 | 26.6 | 24.0 | **260.0** | 10 astropy tasks; pool re-provisioned per task |
| 2026-06-19 | e2e_test.sh | naive | 10 | –/1 | strategy compare | astropy (base cached) | 38.8 | 49.8 | 37.6 | 26.1 | 23.8 | **193.8** | 10 pools pre-warmed up front (replicas=1, MAX_CONCURRENT=1) |
| 2026-06-19 | e2e_test.sh | sliding | 10 | 2/8 | strategy compare | astropy (base cached) | 37.2 | 49.5 | 38.2 | 26.0 | 23.8 | **189.6** | window 2; rolls through 10 images |

### 10-task strategy comparison (2026-06-19) — SUPERSEDED (harness-inflated)

> These numbers were inflated by the test harness, not the platform: every poll
> was a fresh `kubectl` call (~1 s each on GKE: auth-plugin token + TLS + process)
> and poll loops slept 2–4 s. Fixed by running one `kubectl proxy` (auth once) +
> curl to the local API + 0.1 s polls. See **v2** below for real numbers.

Same 10 astropy tasks (offset 0; family base already node-cached), serial
execution (`MAX_CONCURRENT=1`), 3×e2-standard-2.

| Strategy | provision | wait warm | claim | exec | teardown | **TOTAL** |
| :--- | --: | --: | --: | --: | --: | --: |
| none    | 37.8 | **89.2** | 37.5 | 26.6 | 24.0 | **260.0** |
| naive   | 38.8 | 49.8 | 37.6 | 26.1 | 23.8 | **193.8** |
| sliding | 37.2 | 49.5 | 38.2 | 26.0 | 23.8 | **189.6** |

**Findings:**
1. **sliding ≈ naive (~190s) << none (260s).** The differentiator is **wait
   warm**: `none` re-provisions a fresh pool *per task* and serially pays warm-up
   each time (89.2s); naive/sliding amortize it (~49.5s). ⇒ for repeated work,
   any pre-warming beats on-demand.
2. **sliding matches naive here at a fraction of the footprint** — naive held 10
   pools; sliding held ~2. With this family (cheap warms) the latency is the
   same, so sliding is strictly better (same speed, far less idle).
3. **claim/exec/teardown (~37/26/24s) are flat across strategies** — they scale
   ~linearly with the 10 tasks because execution is **serial**. This is the
   ceiling that opt #3 (parallel exec) would cut; warm-pool strategy can't help
   the per-task claim+exec path.
4. All tasks same family (astropy) → base cached → warms are thin-diff (~5s
   each). A cross-family 10-task run would show much larger `wait warm` for
   `none` (one cold base per fresh family) — where pre-pull (#1) + sizing matter
   more.

> Caveat: the e2e releases claims only at final cleanup (not per task), so within
> a run claimed sandboxes accumulate — fine at this scale, but inflates resident
> pods for large serial runs (the Python driver releases per task via
> `sandbox.terminate()`).

### 10-task strategy comparison v2 — fast harness (2026-06-19)

Same workload, after fixing the harness (one `kubectl proxy` + curl to local API
for all hot-path polls/claims; 0.1 s polls; 2-decimal timing). These reflect the
**platform**, not kubectl/poll overhead.

| Strategy | preflight | fetch | ns | provision | wait warm | **claim** | exec | teardown | **TOTAL** |
| :--- | --: | --: | --: | --: | --: | --: | --: | --: | --: |
| none    | 1.67 | 4.34 | 0.93 | 37.42 | 11.63 | **6.12** | 18.11 | 23.91 | **144.24** |
| naive   | 1.63 | 2.89 | 1.00 | 37.52 | 12.54 | **5.70** | 18.34 | 23.99 | **114.43** |
| sliding | 1.68 | 3.36 | 0.97 | 37.69 | 14.66 | **5.75** | 18.26 | 24.09 | **117.24** |

**What changed vs the inflated run (and why):**
- **claim: 37.5 s → ~6 s** (≈ **0.6 s/task**) — claiming a pre-warmed sandbox is
  ~sub-second; the old number was 3 kubectl calls/task + `sleep 2`. *This confirms
  warm-pool claims are effectively instant.*
- **wait warm: 49–89 s → 12–15 s** — the old `wait_pool_ready` used `sleep 4` +
  kubectl polls; real same-family (cached-base) warm-up is ~1 s/pool.
- **TOTALs: 190–260 s → 114–144 s.**

**Where the time actually is now (all legitimate kubectl-bound work, not poll
artifacts):**
- **provision ~37 s** = 10× (`kubectl apply` template + warmpool) ≈ 20 kubectl
  calls @ ~1 s. Could be cut by POSTing creates via the proxy too.
- **teardown ~24 s** = kubectl deletes per pool.
- **exec ~18 s** = 10× `kubectl exec` (~1.8 s each; SPDY/exec stays on kubectl).
- **claim ~6 s** = the only phase now near its true floor.

**Ranking holds:** naive ≈ sliding (~115 s) < none (144 s); sliding matches naive
at a fraction of the warm footprint. The serial ceiling is now provision +
teardown + exec (kubectl-bound), addressable by proxy-POST creates and opt #3
(parallel exec) — *not* warm-pool strategy.

### Footprint: SandboxClaims + warm replicas (10 tasks, 2026-06-19)

`e2e_test.sh` now reports these counters. 10 astropy tasks (1:1 image:task), so
each strategy starts 10 claims and provisions 10 warm replicas total; the
**peak** concurrent warm replicas is the real differentiator.

| Strategy | TOTAL | SandboxClaims started | warm replicas (total) | warm replicas (peak) |
| :--- | --: | --: | --: | --: |
| none    | 142.69 | 10 | 10 | **1** |
| naive   | 112.03 | 10 | 10 | **10** |
| sliding | 111.99 | 10 | 10 | **2** |

**Reading it:** all three do the same *work* (10 claims, 10 warm pods over the
run); they differ only in **peak idle footprint** — `none` keeps 1 warm at a time
(slowest), `naive` keeps all 10 (fastest, highest reservation), `sliding` keeps
~`window` (=2) for ≈naive speed at a fraction of the footprint. Peak scales with
strategy, not task count: for a fixed `MAX_CONCURRENT`, `naive` peak grows with
the number of unique images while `sliding` stays ≈ window.

### Parallel claim+exec (opt #3) — 2026-06-19

`e2e_test.sh` now runs the claim+exec region in **waves of `MAX_CONCURRENT`**
(`-c` / `MAX_CONCURRENT`), and reports the region's wall-clock separately. naive,
4 astropy tasks:

| MAX_CONCURRENT | claim (Σ) | exec (Σ) | **tasks region (wall)** | TOTAL |
| --: | --: | --: | --: | --: |
| 1 | 2.20 | 7.27 | **9.89** | 54.85 |
| 2 | 2.49 | 7.46 | **5.31** | 53.20 |
| 4 | 3.00 | 7.30 | **2.81** | 48.11 |

**Findings:**
1. **The task region scales ~linearly with concurrency:** wall 9.89 → 5.31 →
   2.81 s for c=1/2/4 (≈1×, 1.9×, 3.5×). Aggregate per-task work (claim+exec Σ ≈
   9.5 s) is unchanged — concurrency overlaps it, as expected.
2. **TOTAL improves only modestly (54.9 → 48.1 s)** because TOTAL is now dominated
   by the **serial** provision (~37 s, kubectl applies) + teardown (~24 s) phases,
   not the task region. Those are the next ceiling (proxy-POST creates / parallel
   provision), *not* warm-pool strategy.
3. **Verified 1:1 needs no deeper pools:** concurrent claims hit *distinct* image
   pools (1 replica each), so c>1 works without raising replicas; deeper pools
   matter only when many concurrent claims target the *same* image (pass@k / RL
   group sampling).
4. Backward compatible: `MAX_CONCURRENT=1` (default) = serial; `tasks region
   (wall)` ≈ claim Σ + exec Σ.

### Pre-pull (opt #1) — findings

DaemonSet pre-pull (`prepull.sh`) vs cold, measured on fresh `django` images
(different repo family from the cached `astropy` ones):

| Path (1 task) | pull cost | where it's paid | TOTAL e2e |
| :--- | --: | :--- | --: |
| Cold (no pre-pull) | **80.9s** | in the claim path (`wait warm`), on 1 node | 108.2 |
| Pre-pull then run | 51.7s + 6.0s | DaemonSet step (all 3 nodes, parallel) + ~pod-start | 34.0 (+51.7 prep) |

**Findings:**
1. **Layer sharing is huge.** A second image in an *already-pulled repo family*
   warms in ~11s (only the thin top layer); the *first* image of a fresh family
   is ~81s. ⇒ pre-pull value is mostly about covering **each unique repo family /
   fresh base**, not every instance.
2. **Pre-pull removes the pull from the claim path:** `wait warm` 80.9s → 6.0s
   (just pod start). Time-to-claimable ~84.6s → ~61s here.
3. **Parallel across nodes + scale-up:** the DaemonSet pulled on all 3 nodes at
   once (51.7s total) and any newly-added node would pull automatically — the
   cold path instead re-pays the pull per new pod landing on an uncached node.
4. **For a single task it's ~break-even; it compounds** with more
   tasks/claims/replicas per image and with node scale-up (pull paid once, reused
   by every later claim on every node).
5. **Caveat:** init containers pull **sequentially per node**, and all images
   accumulate on the node's 100 GB disk (~80 × 1.2 GB ceiling) → pre-pull the
   *batch's* unique families, not all 500.

### Cold-pull baseline (separate observation)
- First-ever pull of a ~1.2 GB SWE-bench image onto a fresh node: **minutes**
  (dominates the `warm` phase). Once node-cached, `warm` ≈ 1 s. → image pre-pull
  (opt #1) targets exactly this.

### Notes on baseline numbers
- `preflight` (~1.8 s), `fetch tasks` (~3–4 s, HF API), `create namespace`
  (~1 s) are fixed per-run overheads not shown in the per-strategy columns above.
- Serial claim+exec: `clm`+`exec` scale ~linearly with task count today
  (opt #3 = parallelize).
- These are **baseline** (no optimizations applied). New rows should set the
  "Opt tested" column to the optimization being measured and keep the same
  cluster/config (or note deltas in Environment).

## How to reproduce a row

```bash
cd agent-sandbox/examples/rl-sandbox-scripts
NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=standard-pool \
  ./e2e_test.sh -s <none|naive|sliding> -n <tasks> [-w <window>] -y
```

Copy the printed `── Benchmark ──` block into a new row above.
