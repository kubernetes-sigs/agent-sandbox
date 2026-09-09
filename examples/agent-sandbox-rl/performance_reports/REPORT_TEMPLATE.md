# <TITLE — e.g. "SWE-bench load test" or "Full SWE-bench benchmark">

> **Run completed:** <YYYY-MM-DD HH:MM TZ> — cluster `<cluster>`, pool `<pool>` (<N>× <machine-type>, <disk>), <cache state: cold nodes / warm node caches / flushed before each strategy>.

<!--
Template distilled from swebench500_all_strats_gvisor1000_warm.md (the
tests/loadtest.py `format_report` output). Sections marked (auto) are emitted
by the harness — don't hand-edit them in generated reports; fill them manually
only for ad-hoc write-ups. Sections marked (manual) are for the author.
-->

## Performance progress at a glance (worst → best)  <!-- (manual) -->

<!-- ASCII bar chart of wall-clock per strategy, worst → best, bars proportional
     (≈46 chars for the slowest; scale the rest). One trailing annotation per bar;
     mark the winner with "◀ <X>× vs <worst>". Follow with 1 short paragraph of
     cross-run context: same batch on prior pools/reports, and what this run isolates. -->

```
<worst-strategy>   ██████████████████████████████████████████████  <t>s   <annotation — the floor>
<strategy>         ██████████                                      <t>s   <annotation>
<best-strategy>    █████                                           <t>s   <annotation>   ◀ <X>× vs <worst>
```

<Cross-run context: cold vs warm on this pool, same batch on prior pools — cite the
source reports so the descent is traceable.>

## Parameters  <!-- (auto) -->

- **images**: `<N>`
- **tasks_per_image**: `<K — 1 = eval shape, >1 = RL rollout shape>`
- **total_tasks**: `<N*K>`
- **strategies**: `<naive,sliding,pipelined,none — order matters if caches are shared>`
- **max_concurrent**: `<mc>`
- **max_warmpool_size**: `<cap>`
- **warm_per_task**: `<True|False>`
- **colocate_replicas**: `<True|False>`
- **window_size**: `<None = planner-chosen>`
- **task_duration_s**: `<0.0 = no-op probe, isolates infra latency>`
- **image_template**: `<image set + environment note, e.g. "(real SWE-bench 500 from AR mirror; <pool spec>; <cache footing>)">`

## Methodology  <!-- (auto; verify the cache-footing sentence matches reality) -->

Each strategy runs the **same** batch — **<N>** distinct container images, **<K>** task(s) per image (**<N*K>** tasks total) — end to end against the live cluster, then tears its pools down before the next strategy starts (so strategies never share warm state).

Per task the harness: (1) ensures a `SandboxTemplate` + `SandboxWarmPool` exist for the task's image — *when/how many* pools are pre-warmed is what the **strategy** decides; (2) **claims** a ready sandbox; (3) runs `process_fn`; (4) releases the sandbox.

Wall-clock is the true end-to-end batch time under `max_concurrent=<mc>`. Per-phase totals are summed across concurrent workers, so they exceed wall-clock — divide by the phase count (`n`) for the average a single task saw. **Efficiency** = fastest strategy's wall ÷ this strategy's wall.

> ⚠️ **Cache footing (state it explicitly):** node containerd caches persist across pool teardown. Say whether this run was cold (fresh/flushed nodes), first-strategy-cold (prior methodology), or all-warm — cross-report comparisons are meaningless without it.

Strategies compared:
- **naive** — all pools warmed up front (peak = #images) — fastest claims, largest footprint.
- **sliding** — a rolling window of warm pools tracks the concurrency frontier.
- **pipelined** — double-buffered sliding window — prefetch window N+1 while window N runs, so image pulls overlap execution; footprint bounded to ≤2 windows.
- **none** — no pre-warming; one pool on demand at a time (window=1, serial) — worst-case baseline.

## Cluster & nodes  <!-- (auto) -->

- `<fleet-name>`: **context**=<kube-context>  **namespace**=<ns>  **k8s_version**=<ver>  **nodes**=<n>  **node_pools**=[<pools>]  **instance_types**=[<types>]  **region**=<region>

### Cluster & node configuration  <!-- (manual — the detail the auto line lacks) -->

`<cluster>` GKE cluster (<ownership/purpose>, project `<project>`, region **<region>**)<, recent changes worth noting — upgrades, repools>.

- **k8s**: `<version>`  ·  **agent-sandbox controller**: `<image tag>` (<registry>)
- **runtime**: <gVisor/runc> (`runtimeClass=<rc>`)  ·  **<image streaming on/off>**
- **image source**: <registry + path> (`imagePullPolicy: <policy>`)
- **per-sandbox resources** (`TemplateSpec.resources`): **cpu `<req>`**, **memory `<req>`** —
  requests only, no limits. CPU budget = <pool vCPU> ÷ <cpu req> = **<n>** pods; kubelet pod
  cap **<n>** (<per-node> − ~<system pods> system ≈ **<usable>** usable) — state which one binds.

| pool | machine | nodes | per-node (allocatable) | role |
|---|---|---:|---|---|
| **`<sandbox-pool>`** | <machine-type> | <n> (<zone spread>) | **~<x> vCPU** · **~<x> GiB** mem · **~<x> GiB** ephemeral (<disk type/size>) · **<x>** pods · taint `<taint>` | sandbox workload |
| `<system-pool>` | <machine-type> | <n> | <allocatable> · runc (untainted) | controller + system |

**Pool totals (<sandbox-pool>, probed):** **<x> vCPU** · **~<x> GiB** ephemeral ·
**<x>** pod cap — the three numbers the capacity planner reads. <Optional: ratio vs the
previous pool for cross-report context.>

## Warm-pool plan (<N> pools)  <!-- (auto; truncated at ~60 rows) -->

| image | pool | replicas | cluster |
|---|---|---:|---|
| `<image-ref>` | `<pool-name>` | <r> | <fleet-name> |
| … | … | … | (+<remainder> more) |

## Results — per stage, per strategy  <!-- (auto) -->

> **wall** is the true end-to-end time for the whole batch. Per-phase columns are **summed across concurrency**, so they exceed the wall-clock — the `avg` figures are the per-task experience. **claim avg/max** is the *time-to-sandbox* (request → ready, claimed sandbox); **efficiency** = fastest strategy's wall ÷ this strategy's wall.

| strategy | wall | prep (create+wait) | pool-ready avg/max | claim avg/max | claims | net task Σ/avg | warm pools (peak/total/created) | ok | efficiency |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| **<strategy>** | <w>s | <p>s | <a>/<m>s | <a>/<m>s | <n> | <Σ>/<avg>s | <peak>/<total>/<created> | <ok>/<total> | <e>% |

## Key findings  <!-- (manual — the part only a human/Claude adds) -->

1. **<Headline: which strategy won and by how much.>**
2. **<Cross-run comparison: against which prior report, what changed (pool, cache footing, code), and what that isolates.>**
3. **<Bottleneck read: which phase dominates wall (pull? orchestration floor? claim tail?) and what lever that suggests next.>**

Caveats: <cache footing, strategy order effects, anything non-apples-to-apples>.

## Metric glossary  <!-- (auto, static) -->

Each line in a strategy's RunReport reads `phase  total_s  (n=count, max=slowest)`:

| field | meaning |
|---|---|
| `total_s` | wall-time **summed across all concurrent workers** spent in this phase |
| `n` | how many times the phase ran (one per pool, or one per task) |
| `max` | the slowest single occurrence — the tail latency for that phase |

| phase | what it measures |
|---|---|
| `preflight` | one-time cluster checks (API reachable, namespace, CRDs) before any work |
| `plan` | compute the image→warm-pool plan (in-memory; ~0s) |
| `create_warmpool` | issue the `SandboxWarmPool` create calls (n = pools created) |
| `wait_pool_ready` | block until a pool replica is Ready — the **image pull + pod start** cost |
| `prefetch` | *(pipelined only)* background warm of the next window, overlapped with execution |
| `claim` | request a sandbox → have a claimed, ready one — **time-to-sandbox** |
| `process` | time inside `process_fn` (the task itself) |
| `release` | return the sandbox / claim to the pool |
| `teardown` | delete pools + templates at the end of the run |

## Full RunReport per strategy  <!-- (auto) -->

### <strategy>
```
  preflight        <t>s  (n=1, max=<t>s)
  plan             <t>s  (n=1, max=<t>s)
  create_warmpool  <t>s  (n=<pools>, max=<t>s)
  wait_pool_ready  <t>s  (n=<pools>, max=<t>s)
  claim            <t>s  (n=<tasks>, max=<t>s)
  process          <t>s  (n=<tasks>, max=<t>s)
  release          <t>s  (n=<tasks>, max=<t>s)
  teardown         <t>s  (n=1, max=<t>s)
  TOTAL wall       <t>s
  claims=<n>  tasks=<ok>ok/<err>err  warm peak=<p>
```

## Image list  <!-- (auto; first 80 + count) -->

```
<image refs…>
... (+<remainder> more)
```
