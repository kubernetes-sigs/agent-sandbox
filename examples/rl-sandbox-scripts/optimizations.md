# rl-tunix SWE-bench warm pools — Optimization ideas

Working notes: a backlog of optimizations for running SWE-bench tasks on Agent
Sandbox warm pools.

## Workflow recap (baseline we're optimizing)

1. **Task source** — `R2E-Gym/SWE-Bench-Verified`; each task has a real
   `docker_image` (`slimshetty/swebench-verified:sweb.eval.x86_64.<instance>`,
   ~1.2 GB).
2. **Template** — per unique image, a `SandboxTemplate` (v1beta1) running the
   image with `sleep infinity` (keepalive).
3. **Warm pool** — `SandboxWarmPool` (`replicas`, `sandboxTemplateRef`);
   controller pre-pulls + keeps N sandboxes Ready.
4. **Claim** — per task, a `SandboxClaim` (`warmPoolRef`) adopts a pre-warmed
   sandbox; the pool spins up a replacement.
5. **Resolve + exec** — claim `status.sandbox.name` → pod via
   `agents.x-k8s.io/pod-name` annotation → `kubectl exec` (router-free).
6. **Release** — delete claim, then tear down pools/templates.

Strategies (`none` / `naive` / `sliding`) differ only in *when* pools exist.

Current measured baseline (3×e2-standard-2, images node-cached): per task the
non-pull phases are ~claim 4s + exec 2.6s; first-run `wait warm (pull)` dominates
(image pull, minutes) and drops to ~1s once cached.

---

## Known quantities & replica sizing (prior knowledge)

**Everything is known before the run.** The dataset is static, so the
orchestrator can enumerate the full task list, the exact `docker_image` per task,
and per-image frequency up front (`eval_deepswe.py` already builds
`unique_images` + `image_totals`). This enables pre-pull / pre-size / pre-warm
with no guessing.

**SWE-Bench-Verified is 1 image : 1 task** — verified: 100 rows → 100 unique
images; 500 tasks → 500 unique images. So `tasks_image = 1` for every image.

**Replicas-per-image is driven by concurrency, not dataset totals:**

| Mode | concurrent uses of an image | replicas/pool |
| :--- | :--- | :--- |
| pass@1 eval (this example) | 1 | **1** |
| pass@k eval | k | **k** |
| RL training (GRPO group sampling) | `group_size` | **group_size** (~8–16) |

- The scaling variable is **how many image-pools are warm at once** = the
  in-flight concurrency budget (`MAX_CONCURRENT`), *not* the 500 total images.
- ⇒ `naive` is wasteful for verified (500 pools × 1); `sliding` is the right fit
  (keep only the in-flight window warm).
- Replicas/pool only exceed 1 when an image **repeats** in the active batch
  (pass@k, RL group sampling, or image-reusing datasets like R2E-Gym training /
  SWE-smith).
- Sizing rule:
  `replicas_image = min(concurrent_demand_image, MAX_CONCURRENT × tasks_image/tasks_total)`.

**Implication for strategy defaults:** for verified eval, prefer `sliding` with
`replicas=1` and a window ≈ `MAX_CONCURRENT`; reserve `replicas>1` for
pass@k / RL runs.

### When/how tunix actually decomposes it (`eval_deepswe.py`)

Both decisions are **precomputed from the static dataset, up front — before any
task runs** (nothing is discovered at runtime):

1. **What images** — at *module load* (lines ~188–196), right after
   `load_dataset`: filter to rows with `docker_image`, slice by `TASKS_LIMIT`,
   then `unique_images = set(e["docker_image"] for e in entries)`.
2. **How many replicas** — at the *start of `run_evaluation()`* (lines ~688–711):
   `image_totals = Counter(e["docker_image"] for e in entries)`, then per image
   `size = min(count, MAX_WARMPOOL_SIZE)`. `naive` sizes all pools up front;
   `sliding` applies the same `image_totals` incrementally as the window rolls
   (lines ~790–804).

So the replica rule today is **`replicas_image = min(tasks_for_that_image,
MAX_WARMPOOL_SIZE)`** — driven by tasks-per-image, capped by the flat cap.

**Gaps in this rule (→ optimization #2):**
- For verified (1:1) every `count == 1` ⇒ every pool is `replicas=1`;
  `MAX_WARMPOOL_SIZE` never bites. Per-image replicas only exceed 1 when an image
  repeats (pass@k / RL group sampling / image-reusing datasets, where the same
  instance appears as multiple `entries`).
- **Not concurrency-aware:** sizing uses *total* tasks-per-image, not in-flight
  concurrency. An image with 50 tasks and `MAX_WARMPOOL_SIZE=32` pre-warms 32
  idle pods even if only 8 run at once. The fix is to size to
  `min(concurrent_demand, MAX_CONCURRENT × tasks_image/tasks_total)` instead of
  raw `count`. Timing of the decomposition is fine; the **sizing formula** is the
  thing to improve.

## Idea backlog

> Template per idea: **Why / How / Impact / Effort / Status / Open questions**

### 1. Image pre-pull (kill cold-start) — IMPLEMENTED + MEASURED
- **Why:** the multi-GB image pull dominates first-use latency and gates warm
  pool readiness; at scale, node scale-ups re-pay it.
- **How:** `prepull.sh` deploys a `DaemonSet` with one init container per unique
  image (no-op cmd, `IfNotPresent`) → every node caches them; waits for all nodes
  ready, times it. `--delete` removes the DS (cached images persist).
- **Measured (fresh django family, 1 task):** `wait warm` **80.9s cold → 6.0s**
  after pre-pull; DaemonSet pull ran across all 3 nodes in 51.7s. See
  `performance.md` → "Pre-pull (opt #1) — findings".
- **Big caveat discovered — layer sharing:** 2nd image of an *already-pulled repo
  family* warms in ~11s (thin top layer); only the *first image of a fresh
  family* is truly cold (~81s). ⇒ pre-pull value ≈ per **unique repo family /
  base**, not per instance.
- **Impact:** High for fresh families / scale-up; ~break-even for a single task
  but **compounds** with #tasks·replicas·nodes.
- **Effort:** Done (Medium).
- **Status:** implemented (`prepull.sh`); measured.
- **Open:** init containers pull sequentially per node; disk ceiling ~80×1.2 GB
  on 100 GB nodes → pre-pull batch families only. Future: GKE Image Streaming /
  secondary boot disk (opts to compare).
- **When the DaemonSet is / isn't the right tool (honest assessment):**
  - *Bought us:* (a) the measurements (81s→6s) and the **layer-sharing** finding
    that reframed #9; (b) a **placement-agnostic + scale-up guarantee** — caches
    on every node incl. newly autoscaled ones, so a warm pod lands hot wherever
    the scheduler puts it. This is its one structural edge over on-demand pull.
  - *Didn't help:* single/few tasks ≈ **break-even** (pulls on all nodes to serve
    a 1-node task; cost just shifts from claim path to prepull step); **over-pulls**
    vs. a low-replica pool's actual node spread; and it's the **wrong tool for
    "all images"** (the #9 disk conundrum).
  - *Right sweet spot:* a **small hot working set you want instant regardless of
    placement and across scale-up** = the **sliding window's active images**, not
    the whole dataset. ⇒ keep it but scope it to the window (#9); for full-dataset
    runs prefer Image Streaming / node partitioning.
  - *Bottom line:* measured single-run win was marginal; value is **structural +
    diagnostic**, and only pays off feeding **many** claims spread across nodes
    (needs the concurrency/parallel-exec context, #2/#3).

### 2. Proportional / concurrency-aware pool sizing — IMPLEMENTED
- **Why:** the baseline `replicas = min(tasks_image, MAX_WARMPOOL_SIZE)` ignores
  concurrency and pre-warms one pod per *task* → many idle sandboxes; and has no
  global budget.
- **How (`sizing.py`):**
  `replicas_image = clamp(round(MAX_CONCURRENT × tasks_image / tasks_total),
  1, min(tasks_image, MAX_WARMPOOL_SIZE))` — depth = the image's share of the
  concurrency budget. Plus `recommend_window()` so `sliding`'s total warm
  footprint ≈ `MAX_CONCURRENT`. Wired into `strategies.py`, `run_swebench.py`
  (`MAX_CONCURRENT` env; `WARMPOOL_WINDOW_SIZE=0` ⇒ auto window) and `e2e_test.sh`
  (`compute_replicas`).
- **Demonstrated (`python sizing.py`):** skewed 8-image batch (100 tasks, cap 32)
  — baseline pre-warms **92 idle pods**; improved footprint is **8 / 11 / 32 / 92**
  at `MAX_CONCURRENT = 1 / 8 / 32 / 256`. Verified (1:1) stays 1 per image, but
  `sliding` window auto-scales 1→8 with the budget.
- **Coupling:** the budget that makes this real is execution concurrency, so it
  pairs with opt #3 (parallel exec). Default `MAX_CONCURRENT=1` (serial) keeps it
  correct today; raise it with #3.
- **Impact:** High for image-repeating / high-concurrency runs; correctness-safe
  for verified.
- **Effort:** Done (Low).
- **Status:** implemented (`sizing.py`), self-demo included; cluster-measure the
  footprint reduction once #3 lands.

### 3. Parallel task execution — IMPLEMENTED (e2e) + MEASURED
- **Why:** claim+exec ran **serially**; wall-clock scaled linearly with tasks.
- **How (`e2e_test.sh`):** `run_units` runs claim+exec in **waves of
  `MAX_CONCURRENT`** (bash 3.2: per-wave `wait <pids>` — never bare `wait`, which
  would block on the `kubectl proxy`); each unit writes results/timings to a temp
  dir, parent aggregates. New `-c`/`MAX_CONCURRENT` knob (also the pool-sizing
  budget). Report adds **tasks region (wall)** vs aggregate claim/exec (Σ).
- **Measured (naive, 4 tasks):** tasks region **9.89→5.31→2.81 s** at c=1/2/4
  (≈1×/1.9×/3.5×); per-task work unchanged. TOTAL 54.9→48.1 s — now bounded by
  the **serial** provision (~37 s) + teardown (~24 s), the next ceiling
  (proxy-POST creates). See `performance.md`.
- **Impact:** High for multi-task task region; gated by provision/teardown for TOTAL.
- **Effort:** Done (Medium).
- **Status:** implemented in `e2e_test.sh`; the Python/async path lands in
  `agent-sandbox-rl` (`AsyncSandboxFleet`).
- **Open (resolved):** per-phase timers under concurrency → report the region's
  wall-clock + aggregate sums.

### 4. Reuse a sandbox for multiple tasks  — DEPRIORITIZED (niche + risky)
- **Why considered:** claim-per-task churns sandboxes (each claim consumes a warm
  one and forces a replacement spin-up).
- **Reality check:** the model **dirties** the pod every run (edits `/testbed`,
  runs tests, installs deps). So reuse needs a reset between runs.
  - For **SWE-Bench-Verified it doesn't apply at all**: 1:1 image:task, each task
    is a *different image/repo* — nothing to reuse across tasks.
  - Only applies to **repeated runs of the same task** (RL group sampling /
    pass@k: same image + same clean `base_commit`).
- **How (if ever):** keep one claim for G rollouts of the same instance, reset
  between each: `git -C /testbed reset --hard <base_commit> && git -C /testbed
  clean -fdx`.
- **Risk:** reset only restores the git tree, not state *outside* the repo (pip
  installs, caches, `/tmp`, DBs) → **state bleed can corrupt reward**. A fresh
  claimed sandbox guarantees a pristine baseline.
- **Verdict:** fresh-per-task/rollout is the safe correctness default. Skip unless
  claim churn is proven to be a real bottleneck in the group-sampling path.
- **Status:** deprioritized.

### 5. Autoscale pools on demand (HPA)
- **Why:** static replicas can't track bursty claim rates.
- **How:** HPA on the controller's claim-rate metric (see
  `../hpa-swp-scaling`); scale warm pools to maintain a target claim rate.
- **Impact:** Medium/High at scale.
- **Effort:** Medium (needs managed Prometheus + custom metrics adapter).
- **Status:** reference example exists upstream.

### 6. Faster / cheaper readiness + lifecycle
- **Why:** we poll `readyReplicas` / Sandbox `Ready` on a fixed interval;
  abandoned claims linger.
- **How:** add a container `readinessProbe` so Ready means "usable"; use the
  SDK's `shutdown_after_seconds` (claim TTL → auto-delete) for crash-safety;
  consider `kubectl wait --for=condition=Ready` vs custom poll.
- **Impact:** Low/Medium (snappier, self-healing).
- **Effort:** Low.

### 7. Right-size sandbox resources
- **Why:** template requests `250m / 512Mi`; real SWE-bench builds/tests may
  need more, and over-request limits pods-per-node density.
- **How:** profile per repo; set requests/limits per image class.
- **Impact:** Medium (density vs OOM/throttle trade-off).
- **Effort:** Low/Medium.

### 8. gVisor isolation (correctness/security, not speed)
- **Why:** untrusted, model-generated code should be isolated.
- **How:** gVisor-enabled node pool + `RUNTIME_CLASS=gvisor`.
- **Impact:** Security (note: gVisor adds some runtime overhead).
- **Effort:** Medium (infra).
- **Status:** supported via env, not enabled on the current cluster.

### 9. Working-set pre-pull for massive runs (disk-bounded)
- **Conundrum:** for a run over *all* images, "pre-pull every image on every
  node" (current `prepull.sh`) cannot fit — 500 verified × ~1.2 GB ≈ 600 GB vs
  ~100 GB node disk (worse for R2E-Gym/SWE-smith's thousands).
- **Reframe:** a node only needs images for pods scheduled **on it**, so the unit
  is a **per-node working set**, not the whole batch. And node disk = **sum of
  unique layers** (family base stored once + thin per-instance diffs), not Σ full
  image sizes — so the ~12 family bases dominate, not the 500 tags.
- **How:**
  1. **Pre-pull follows the sliding window**, not the batch: pull only the active
     window's images; as it slides, pull the next and let finished ones go.
     (Evolve `prepull.sh` to take the window, not a fixed list.)
  2. **Lean on kubelet image GC:** unused images are LRU-evicted past the disk
     high-threshold (~85% on GKE); a live warm pool pins its image, a torn-down
     pool's image becomes evictable. Window-follow + GC = self-managing cache.
  3. **Working-set constraint:** `window × (family_base + diffs) ≲ node_disk ×
     gc_low_threshold` — bounds window width (ties to the concurrency/sizing
     budget, #2).
- **Scale answers (compare later):** **GKE Image Streaming (gcfs)** — lazy
  layer streaming + bounded cache, removes the "fit everything" problem entirely;
  **node partitioning by repo family** (nodeSelector/affinity per template) so
  each node holds a few families; **secondary boot disk** baking family bases.
- **Impact:** required for full-dataset runs (otherwise disk is the hard gate).
- **Effort:** Medium (window-follow prepull) / Medium-High (streaming, partitioning).
- **Status:** idea (recorded). Depends on / extends #1 (pre-pull) and #2 (sizing).
- **Open:** measure real per-node unique-layer footprint per family; pick window
  width from disk budget; evaluate Image Streaming vs DaemonSet pre-pull.

### 10. In-pod multi-tenancy — multiple executors per pod
- **Source:** AI21 "Scaling Agentic Evaluation" (200k+ SWE-bench runs). Their 10×
  lever is "multiple executors per pod": ~500 pods (one per instance) provisioned
  *once* and reused across dozens of runs, with trajectories isolated *logically*
  inside the pod (session-aware MCP server), not physically.
- **Why it's big here:** decouples **pods from concurrency** —
  `pods_needed = ceil(concurrency / executors_per_pod)`. One warm sandbox per
  image can serve G concurrent trajectories ⇒ **1 image resident per instance,
  not G** → directly shrinks the resident-image footprint (solves the #9 disk
  conundrum), amortizes per-instance setup (checkout/deps/server), and raises
  density. This is the concurrent-isolation answer to #4 (which I'd deprioritized
  for sequential-reset).
- **How to isolate sessions in one pod (no interference):**
  - **Repo:** `git worktree add --detach /work/<sid> <base_commit>` per session —
    own working tree, shared `.git` objects (cheap); edits + test caches stay in
    the worktree.
  - **Env:** per-session `HOME`, `TMPDIR`, `XDG_*` → `/work/<sid>/…`.
  - **Deps:** rely on read-mostly shared site-packages; per-session venv overlay
    only if a task mutates the env.
  - **Servers/ports/DBs:** per-session instances on distinct ports/sockets, or a
    **session-aware server** (AI21's MCP extension).
  - Executor = a `kubectl exec` session with CWD+HOME pinned to its worktree.
- **Trade-offs (honest):** logical isolation ≠ security isolation — untrusted
  RL-generated code can reach sibling worktrees / shared deps, so this is weaker
  than one-sandbox-per-trajectory + gVisor. Best for *trusted-ish eval at scale*;
  keep per-trajectory sandboxes where hard isolation matters. Also: blast radius
  (one pod crash kills G sessions) and in-pod CPU/mem contention (cap
  `executors_per_pod`).
- **Impact:** High at scale (density + disk). **Effort:** High (session manager,
  worktree lifecycle, optional session-aware server).
- **Status:** idea (recorded). Interacts with #2 (sizing), #4 (reuse), #9 (disk).
- **Open:** worktree/overlay behavior under gVisor; deps-mutation cases; how to
  expose per-session isolation through the agent-sandbox claim model (1 claim →
  N sessions).

### 10.1. Process-isolation tier (bubblewrap / nsjail)
- **Idea:** a middle isolation tier for #10's executors-per-pod — stronger than
  worktrees, far lighter than nested VMs (#10.2). Run each executor under a
  lightweight sandboxer using **Linux namespaces + seccomp + cgroups/rlimits**.
- **Tools:** **bubblewrap** (`bwrap`, unprivileged, used by Flatpak), **nsjail**
  (Google; adds rlimits/cgroups), firejail. (Not a restricted shell like `rbash`
  — that's a convenience boundary, not isolation.)
- **What each executor gets:** mount confinement (own `/testbed` worktree, RO
  base, private `/tmp` `/proc` — real fs-escape block, not convention); PID/IPC
  ns (no sibling visibility/signals); seccomp syscall filter; per-executor
  CPU/mem/fd caps (also fixes in-pod contention).
- **Isolation level:** shared-host-kernel. worktree < **bwrap/nsjail** < gVisor <
  Kata/Firecracker. Raises the bar a lot, but a **kernel exploit still escapes** →
  not sufficient alone for hostile code.
- **Catches:** needs **unprivileged user namespaces** (or `CAP_SYS_ADMIN`) in the
  pod securityContext — default k8s seccomp/AppArmor / restricted PSS often block
  `clone(CLONE_NEWUSER)`, so you must relax it (trades host isolation). **Does not
  stack on gVisor** cleanly (runsc nested-userns limits) — it's an *alternative*
  to gVisor, not a layer on it.
- **Upside vs #10.2:** makes "router-to-inside" *easier* — executors stay normal
  processes (shared netns), reachable via the normal exec path, no inner-VM
  network demux. Flip side: shared netns = weaker per-session network isolation.
- **Impact:** Medium/High for *trusted-ish* eval at density (defense-in-depth,
  cheap). **Effort:** Medium.
- **Status:** idea (recorded). Tier between #10 (logical) and #10.2 (microVM).

### 10.2. Secure in-pod multi-tenancy via nested isolation
- **Idea:** keep #10's density (multiple executors per pod) but replace its weak
  *logical* isolation (worktrees) with **hardware/container-level isolation per
  executor** — so it's safe even for untrusted RL-generated code. Each executor
  runs in its own nested boundary inside the outer Sandbox pod.
- **Isolation options (weak → strong):**
  - **Docker-in-Docker (DinD):** outer pod runs a daemon; each executor = an inner
    container (namespace/cgroup isolation). Needs a privileged outer pod →
    weakens the host boundary; cheapest.
  - **Nested Kata / Firecracker microVMs:** each executor in its own lightweight
    VM (hardware-virt isolation). Strongest. agent-sandbox already demonstrates
    Kata: `examples/kata-gke-sandbox` (`runtimeClassName: kata-qemu`,
    kata-deploy) and `examples/vscode-sandbox/overlays/{kata,kata-mshv}`.
    Firecracker is available as a Kata hypervisor backend (`kata-fc`).
- **Why:** combines #10's pod/disk/density win (1 image-resident per instance,
  N executors) with **per-executor hard isolation** — removes #10's main
  caveat. Also reduces blast radius (an inner crash is contained; only the outer
  pod failing kills all).
- **The thing to solve — "router to the inside":** the agent-sandbox SDK/router
  addresses the *outer* Sandbox (one pod, one network identity), but each inner
  microVM/container has its own netns. We need to **route external per-session
  traffic → outer pod → the specific inner executor**: a session-aware
  demux/proxy inside the outer pod (session-id → inner VM endpoint), or extend
  the router with inner-endpoint registration. (AI21 solved the *logical* version
  at the MCP layer; here it's a real *network* routing problem.)
- **Requirements/caveats:** nested virtualization for Kata-in-pod (KVM / nested
  virt; GKE Sandbox-capable nodes / machine types); DinD privileged trade-off;
  per-microVM CPU/mem overhead (cap executors/pod); still one outer-pod blast
  radius.
- **Impact:** High — secure multi-tenancy at density (best of #10 + strong
  isolation). **Effort:** High (nesting setup + router-to-inside).
- **Status:** idea (recorded). Variant of #10; builds on the Kata examples;
  the open piece is router-to-inside.

### 11. Decouple generation from evaluation (resumability)
- **Source:** same AI21 post (the "reliability tax" at 200k runs).
- **Why:** losing a long trajectory near completion wastes tokens/compute; failed
  *evaluation* shouldn't force re-*generation*.
- **How:** split the pipeline into Generation (produce the patch) and Evaluation
  (run tests), **persist generation artifacts** (patch + metadata), so a failed
  eval retries cheaply and partial data (e.g. 80% done) is analyzable without
  waiting for 100%.
- **Related lesson — never re-fetch per pod:** AI21's first attempt died on
  HuggingFace **429s** from thousands of pods re-downloading the dataset; our
  analog is the **Docker Hub 429** we hit. Pre-stage images/data once (see #1),
  never per-pod.
- **Impact:** Medium/High (reliability + cost at scale). **Effort:** Medium.
- **Status:** idea (recorded).

> Ref (#10, #11): AI21, "Scaling Agentic Evaluation on SWE-bench" —
> https://www.ai21.com/blog/scaling-agentic-evaluation-swe-bench/

### 12. Mirror to Artifact Registry + GKE Image Streaming
- **Source:** Nebius/SWE-rebench (internal **mirrors** to beat PyPI/Docker Hub/
  Ubuntu rate limits) + GKE-native image features. Addresses three problems we
  hit at once: Docker Hub **429s**, cold-pull latency (#1), and the per-node
  **disk** ceiling (#9).
- **Two parts that compose (and must, on GKE):**
  1. **Mirror images into Artifact Registry (AR).** Either an AR **remote
     repository** (transparent pull-through cache of `docker.io`), or an explicit
     one-time **copy** of the batch's tags (`gcrane/crane copy`,
     `slimshetty/...` → `us-central1-docker.pkg.dev/<proj>/<repo>/...`). Removes
     Docker Hub rate limits and puts bytes in-region (fast).
  2. **GKE Image Streaming (Container File System / gcfs).** Enable on the node
     pool; pods **start before the full image is pulled**, layers stream lazily
     on first read into a **bounded, evicting local cache**.
  - **Hard dependency:** Image Streaming **only works for images in Artifact
    Registry** — so the mirror (part 1) is a *prerequisite* for streaming.
- **Why this is the strong answer:**
  - Kills the **429** (mirror) → makes the #9 GC-churn / re-pull model safe.
  - Near-zero **cold start** without an explicit pre-pull DaemonSet — streaming
    largely **obviates #1** (no full pull to pre-do); pods become claimable as
    soon as the entrypoint's first reads stream in.
  - **Disk:** the node holds only the **blocks actually read**, evicted under
    pressure — directly dissolves the "fit all images" conundrum (#9); no manual
    working-set window needed for the *bytes* (you may still pre-warm pools for
    *readiness*).
- **Caveats:** GKE-only (gcfs feature on the node pool); first read of a
  not-yet-streamed layer pays some latency (mitigated by warm pools / a small
  pre-pull of hot images like the django base); AR storage/egress cost; remote
  repo needs config + auth. Streaming wins most when a pod reads a *fraction* of
  the image (true for startup; test runs read more).
- **Suggested combo:** AR remote repo (no 429) + Image Streaming (no disk/pull
  gate) + light pre-pull/warm of the **django base** (46% of tasks, #1) + warm
  pools for *readiness*. This makes #9's window/GC management mostly unnecessary.
- **Impact:** High (one move addresses 429 + cold-start + disk on GKE).
- **Effort:** Low–Medium (enable gcfs; set up AR remote repo or a copy job).
- **Status:** idea (recorded). Supersedes much of #1/#9 *on GKE*; #1's DaemonSet
  remains the portable (non-GKE) fallback.
- **Open:** measure streamed cold-start vs DaemonSet pre-pull on our cluster;
  remote-repo vs explicit-copy trade-off; AR cost for the 500-image set.

> Ref (#12): Nebius, "The infrastructure behind SWE-rebench" —
> https://nebius.com/blog/posts/infrastructure-behind-swe-rebench

---

## Decisions / changelog

- _(none yet)_
