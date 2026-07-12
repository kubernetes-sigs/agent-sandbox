# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""SWE-bench-style load test for the fleet.

A parameterized harness that simulates a SWE-bench batch — *N* distinct images,
*K* tasks each — runs one or more warm-pool strategies against a live cluster, and
emits a full report (cluster/nodes, image list, warm-pool plan, and a per-stage
benchmark for every strategy). It is **not** a unit test (needs a real cluster); the
filename avoids pytest's ``test_*.py`` collection. Pure helpers below are unit-tested
in ``test_loadtest.py``.

Example:
    python -m tests.loadtest \\
        --images 50 --tasks-per-image 2 --strategies all \\
        --image-template 'us-docker.pkg.dev/PROJ/swebench-mirror/bench:img{i:03d}' \\
        --context gke_PROJ_REGION_CLUSTER --namespace agent-sandbox-rl \\
        --node-selector cloud.google.com/gke-nodepool=pool-fat \\
        --max-concurrent 40 --task-duration 0
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import time

from agent_sandbox_rl import (AsyncSandboxFleet, ClusterConfig, FleetConfig,
                              ListSource, STRATEGIES, Task, TemplateSpec)

ALL_STRATEGIES = ["none", "naive", "sliding", "pipelined"]


# --------------------------------------------------------------------------- #
# Pure helpers (unit-tested without a cluster)
# --------------------------------------------------------------------------- #
def build_tasks(images: int, tasks_per_image: int, image_template: str) -> list[Task]:
    """``images`` distinct image refs (``image_template`` formatted with ``i``),
    each carrying ``tasks_per_image`` tasks → ``images*tasks_per_image`` tasks."""
    if images < 1 or tasks_per_image < 1:
        raise ValueError("images and tasks_per_image must be >= 1")
    refs = [image_template.format(i=i) for i in range(images)]
    if images > 1 and len(set(refs)) == 1:
        raise ValueError(
            f"image_template {image_template!r} produced {images} identical refs — "
            "include an '{i}' placeholder so the images are distinct")
    out: list[Task] = []
    for i, img in enumerate(refs):
        for k in range(tasks_per_image):
            out.append(Task(id=f"img{i:04d}-t{k}", image=img))
    return out


def parse_node_selector(items: list[str] | None) -> dict | None:
    """``["k=v", ...]`` -> ``{"k": "v"}`` (or None)."""
    if not items:
        return None
    sel = {}
    for it in items:
        if "=" not in it:
            raise ValueError(f"node-selector must be key=value, got {it!r}")
        k, v = it.split("=", 1)
        sel[k] = v
    return sel


def resolve_strategies(spec: str) -> list[str]:
    if spec == "all":
        return list(ALL_STRATEGIES)
    out = [s.strip() for s in spec.split(",") if s.strip()]
    bad = [s for s in out if s not in STRATEGIES]
    if bad:
        raise ValueError(f"unknown strategies {bad}; choose from {sorted(STRATEGIES)}")
    return out


def _phase(rep: dict, name: str) -> dict:
    return rep.get("phases", {}).get(name, {"count": 0, "total_s": 0.0, "max_s": 0.0})


def stage_metrics(result: dict) -> dict:
    """Derive the headline per-stage numbers from a result's RunReport dict.

    Phase totals are *summed across concurrency*, so they exceed wall-clock; that's
    expected and noted in the report."""
    rep = result["report"]
    create = _phase(rep, "create_warmpool")
    wait = _phase(rep, "wait_pool_ready")
    prefetch = _phase(rep, "prefetch")
    claim = _phase(rep, "claim")
    process = _phase(rep, "process")
    n_wait = wait["count"] or 1
    n_claim = claim["count"] or 1
    n_proc = process["count"] or 1
    return {
        "wall_s": result["wall_s"],
        "prep_create_s": create["total_s"],
        "prep_wait_s": wait["total_s"],
        "prefetch_s": prefetch["total_s"],
        "wait_avg_s": wait["total_s"] / n_wait,
        "wait_max_s": wait["max_s"],
        "claims": claim["count"],
        "claim_avg_s": claim["total_s"] / n_claim,  # avg time-to-sandbox per task
        "claim_max_s": claim["max_s"],              # slowest single claim (tail)
        "net_task_s": process["total_s"],          # time in process_fn after sandbox ready
        "net_task_avg_s": process["total_s"] / n_proc,
        "warm_pools_created": create["count"],
        "warm_peak": rep.get("warm_replicas_peak", 0),
        "warm_total": rep.get("warm_replicas_total", 0),
        "ok": result["ok"],
        "total": result["total"],
    }


def _fmt_env(env: dict) -> str:
    if not env:
        return "_(unavailable)_"
    lines = []
    for cname, info in env.items():
        bits = []
        for k in ("context", "namespace", "k8s_version", "nodes", "node_pools",
                  "instance_types", "region"):
            v = info.get(k)
            if v not in (None, [], ""):
                if isinstance(v, list):
                    v = "[" + ", ".join(str(x) for x in v) + "]"
                bits.append(f"**{k}**={v}")
        lines.append(f"- `{cname}`: " + "  ".join(bits))
    return "\n".join(lines)


_STRATEGY_BLURB = {
    "none": "no pre-warming; one pool created on demand, peak footprint 1 "
            "(cold baseline).",
    "naive": "all pools warmed up front (peak = #images) — fastest claims, "
             "largest footprint.",
    "sliding": "a rolling window of warm pools tracks the concurrency frontier.",
    "pipelined": "double-buffered sliding window — prefetch window N+1 while window N "
                 "runs, so image pulls overlap execution; footprint bounded to ≤2 windows.",
}

_GLOSSARY = """\
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
| `process` | time inside `process_fn` (the task itself; 0s here by design) |
| `release` | return the sandbox / claim to the pool |
| `teardown` | delete pools + templates at the end of the run |"""


def _methodology(params: dict) -> str:
    imgs = params.get("images", "N")
    per = params.get("tasks_per_image", "K")
    total = params.get("total_tasks", "NxK")
    mc = params.get("max_concurrent", "?")
    dur = params.get("task_duration_s", 0)
    work = (f"a no-op probe (`task_duration={dur}s`), so the table isolates "
            "*infrastructure* latency from task compute"
            if not dur else f"a probe that sleeps `task_duration={dur}s`")
    strategies = [s.strip() for s in str(params.get("strategies", "")).split(",")
                  if s.strip()]
    lines = [
        f"Each strategy runs the **same** synthetic SWE-bench batch — **{imgs}** "
        f"distinct container images, **{per}** task(s) per image (**{total}** tasks "
        "total) — end to end against the live cluster, then tears its pools down "
        "before the next strategy starts (so strategies never share warm state).",
        "",
        "Per task the harness: (1) ensures a `SandboxTemplate` + `SandboxWarmPool` "
        "exist for the task's image — *when/how many* pools are pre-warmed is what the "
        f"**strategy** decides; (2) **claims** a ready sandbox; (3) runs `process_fn` — "
        f"here {work}; (4) releases the sandbox.",
        "",
        f"Wall-clock is the true end-to-end batch time under "
        f"`max_concurrent={mc}`. Per-phase totals are summed across concurrent workers, "
        "so they exceed wall-clock — divide by the phase count (`n`) for the average a "
        "single task saw. **Efficiency** = fastest strategy's wall ÷ this strategy's wall.",
    ]
    if params.get("warm_per_task") or params.get("colocate_replicas"):
        modes = []
        if params.get("warm_per_task"):
            modes.append("**per-task sizing** (one warm replica per task — "
                         f"`min(tasks_image, max_warmpool_size={params.get('max_warmpool_size', 8)})`"
                         " — so claims are near-instant)")
        if params.get("colocate_replicas"):
            modes.append("**replica co-location** (a pool's replicas prefer one "
                         "node, so only the first pulls the image and the rest "
                         "start from the node layer cache)")
        lines.append("")
        lines.append("RL instant-claim mode: " + "; ".join(modes) + ".")
    if strategies:
        lines.append("")
        lines.append("Strategies compared:")
        for s in strategies:
            if s in _STRATEGY_BLURB:
                lines.append(f"- **{s}** — {_STRATEGY_BLURB[s]}")
    return "\n".join(lines)


def format_report(params: dict, plan_entries: list[dict], env: dict,
                  results: list[dict], images: list[str]) -> str:
    """Render the full markdown report. ``results`` are per-strategy dicts."""
    fastest = min((stage_metrics(r)["wall_s"] for r in results if r["ok"] == r["total"]),
                  default=None)
    L = []
    L.append("# SWE-bench load test\n")
    L.append("## Parameters\n")
    for k in ("images", "tasks_per_image", "total_tasks", "strategies",
              "max_concurrent", "max_warmpool_size", "warm_per_task",
              "colocate_replicas", "window_size", "task_duration_s",
              "image_template"):
        if k in params:
            L.append(f"- **{k}**: `{params[k]}`")
    L.append("")
    L.append("## Methodology\n")
    L.append(_methodology(params))
    L.append("")
    L.append("## Cluster & nodes\n")
    L.append(_fmt_env(env))
    L.append("")
    L.append(f"## Warm-pool plan ({len(plan_entries)} pools)\n")
    L.append("| image | pool | replicas | cluster |")
    L.append("|---|---|---:|---|")
    for e in plan_entries[:60]:
        img = e["image"].rsplit("/", 1)[-1]
        L.append(f"| `{img}` | `{e['pool']}` | {e['replicas']} | {e['cluster']} |")
    if len(plan_entries) > 60:
        L.append(f"| … | … | … | (+{len(plan_entries) - 60} more) |")
    L.append("")
    L.append("## Results — per stage, per strategy\n")
    L.append("> **wall** is the true end-to-end time for the whole batch. Per-phase "
             "columns are **summed across concurrency**, so they exceed the wall-clock — "
             "the `avg` figures are the per-task experience. **claim avg/max** is the "
             "*time-to-sandbox* (request → ready, claimed sandbox); **efficiency** = "
             "fastest strategy's wall ÷ this strategy's wall.\n")
    hdr = ("| strategy | wall | prep (create+wait) | pool-ready avg/max | "
           "claim avg/max | claims | net task Σ/avg | "
           "warm pools (peak/total/created) | ok | efficiency |")
    L.append(hdr)
    L.append("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
    for r in results:
        m = stage_metrics(r)
        eff = (f"{100*fastest/m['wall_s']:.0f}%"
               if fastest is not None and m["wall_s"] and m["ok"] == m["total"]
               else "—")
        L.append(
            f"| **{r['strategy']}** | {m['wall_s']:.1f}s | "
            f"{m['prep_create_s'] + m['prep_wait_s']:.1f}s | "
            f"{m['wait_avg_s']:.1f}/{m['wait_max_s']:.0f}s | "
            f"{m['claim_avg_s']:.1f}/{m['claim_max_s']:.0f}s | {m['claims']} | "
            f"{m['net_task_s']:.1f}/{m['net_task_avg_s']:.2f}s | "
            f"{m['warm_peak']}/{m['warm_total']}/{m['warm_pools_created']} | "
            f"{m['ok']}/{m['total']} | {eff} |")
    L.append("")
    L.append("## Metric glossary\n")
    L.append(_GLOSSARY)
    L.append("")
    L.append("## Full RunReport per strategy\n")
    for r in results:
        L.append(f"### {r['strategy']}\n```")
        rep = r["report"]
        if rep.get("error"):
            L.append(f"  ERROR: {rep['error']}")
        for ph, c in rep.get("phases", {}).items():
            L.append(f"  {ph:<16} {c['total_s']:8.2f}s  (n={c['count']}, max={c['max_s']:.2f}s)")
        L.append(f"  {'TOTAL wall':<16} {r['wall_s']:8.2f}s")
        L.append(f"  claims={rep.get('claims')}  tasks={rep.get('tasks_ok')}ok/"
                 f"{rep.get('tasks_err')}err  warm peak={rep.get('warm_replicas_peak')}")
        L.append("```\n")
    L.append("## Image list\n")
    L.append("```")
    for img in images[:80]:
        L.append(img)
    if len(images) > 80:
        L.append(f"... (+{len(images) - 80} more)")
    L.append("```")
    return "\n".join(L)


# --------------------------------------------------------------------------- #
# Live run (needs a cluster)
# --------------------------------------------------------------------------- #
def _make_fleet(args) -> AsyncSandboxFleet:
    cluster = ClusterConfig(
        name="loadtest",
        context=args.context,
        namespace=args.namespace,
        node_selector=parse_node_selector(args.node_selector),
        runtime_class=args.runtime_class,
    )
    cfg = FleetConfig(
        clusters=[cluster],
        max_concurrent=args.max_concurrent,
        max_warmpool_size=args.max_warmpool_size,
        window_size=args.window_size,
        warm_per_task=args.warm_per_task,
        ready_timeout=args.ready_timeout,
        template=TemplateSpec(runtime_class=args.runtime_class,
                              colocate_replicas=args.colocate),
    )
    return AsyncSandboxFleet(cfg)


async def _run_strategy(args, tasks, strategy):
    dur = args.task_duration

    async def probe(task, handle):
        if dur > 0:
            await asyncio.sleep(dur)        # simulate per-task work
        return handle.pod_name

    fleet = _make_fleet(args)
    entries = []
    t0 = time.monotonic()
    try:
        fleet.load_tasks(ListSource(tasks))
        plan = await fleet.plan()          # capture the warm-pool plan
        entries = [{"image": e.image, "pool": e.pool, "replicas": e.replicas,
                    "cluster": e.cluster} for e in plan.entries]
        t0 = time.monotonic()
        res = await fleet.run(probe, strategy=strategy, concurrency=args.max_concurrent)
        wall = time.monotonic() - t0
        ok = sum(1 for r in res if not isinstance(r, Exception))
        report = fleet.report.to_dict()
    except Exception as e:                  # noqa: BLE001 — record the failure, keep going
        wall = time.monotonic() - t0
        ok, res = 0, []
        report = (fleet.report.to_dict() if fleet.report else {"phases": {}})
        report["error"] = f"{type(e).__name__}: {e}"
    finally:
        fleet.close()
    return ({"strategy": strategy, "wall_s": round(wall, 2), "ok": ok,
             "total": len(tasks), "report": report}, entries)


async def _main(args):
    strategies = resolve_strategies(args.strategies)
    tasks = build_tasks(args.images, args.tasks_per_image, args.image_template)
    images = sorted({t.image for t in tasks})
    print(f"load test: {len(images)} images x {args.tasks_per_image} = {len(tasks)} tasks; "
          f"strategies={strategies}", flush=True)

    results, plan_entries, env = [], [], {}
    for strat in strategies:
        print(f"  running {strat} …", flush=True)
        result, entries = await _run_strategy(args, tasks, strat)
        results.append(result)
        plan_entries = plan_entries or entries
        env = env or result["report"].get("environment", {})
        m = stage_metrics(result)
        print(f"    {strat}: wall={m['wall_s']:.1f}s ok={m['ok']}/{m['total']} "
              f"peak={m['warm_peak']}", flush=True)

    params = {
        "images": args.images, "tasks_per_image": args.tasks_per_image,
        "total_tasks": len(tasks), "strategies": ",".join(strategies),
        "max_concurrent": args.max_concurrent,
        "max_warmpool_size": args.max_warmpool_size,
        "warm_per_task": args.warm_per_task, "colocate_replicas": args.colocate,
        "window_size": args.window_size,
        "task_duration_s": args.task_duration, "image_template": args.image_template,
    }
    md = format_report(params, plan_entries, env, results, images)
    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    with open(args.out, "w") as fh:
        fh.write(md)
    with open(args.out.rsplit(".", 1)[0] + ".json", "w") as fh:
        json.dump({"params": params, "plan": plan_entries, "environment": env,
                   "results": results}, fh, indent=2)
    print(f"\nwrote {args.out}", flush=True)
    print("\n" + md.split("## Results")[1].split("## Full")[0], flush=True)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="SWE-bench-style fleet load test.")
    p.add_argument("--images", type=int, required=True, help="number of distinct images")
    p.add_argument("--tasks-per-image", type=int, default=1, help="tasks per image")
    p.add_argument("--strategies", default="all",
                   help="'all' or comma list: none,naive,sliding,pipelined")
    p.add_argument("--image-template", required=True,
                   help="image ref with {i}, e.g. .../bench:img{i:03d}")
    p.add_argument("--max-concurrent", type=int, default=40)
    p.add_argument("--max-warmpool-size", type=int, default=8,
                   help="hard cap on replicas per pool (raise for warm-per-task)")
    p.add_argument("--warm-per-task", action="store_true",
                   help="warm one replica per task (instant claims; needs capacity)")
    p.add_argument("--colocate", action="store_true",
                   help="prefer co-locating a pool's replicas on one node (cache reuse)")
    p.add_argument("--window-size", type=int, default=None)
    p.add_argument("--ready-timeout", type=int, default=1800)
    p.add_argument("--task-duration", type=float, default=0.0,
                   help="seconds to sleep in process_fn (simulate task work)")
    p.add_argument("--context", default=None, help="kube context (default: ambient)")
    p.add_argument("--namespace", default="default")
    p.add_argument("--node-selector", action="append",
                   help="key=value (repeatable) to pin sandboxes to a node pool")
    p.add_argument("--runtime-class", default=None, help="e.g. gvisor")
    p.add_argument("--out", default=f"performance_reports/loadtest_{int(time.time())}.md")
    return p


def main(argv=None):
    asyncio.run(_main(build_parser().parse_args(argv)))


if __name__ == "__main__":
    main()
