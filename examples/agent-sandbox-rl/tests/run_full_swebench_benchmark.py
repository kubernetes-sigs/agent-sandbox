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

"""Capacity-aware planner + runner for the full SWE-bench batch.

Reads a node pool's CPU + ephemeral storage, computes the **optimal preload plan**
for running all *N* SWE-bench tasks (max concurrency, strategy, per-image replicas /
window so every image is pulled + *uncompressed* and the sandboxes are warm **before**
the task phase starts), then optionally runs it — reporting **preload** vs **task**
time separately.

SWE-bench is 1 image : 1 task (each instance ships its own image), so the lever is
concurrency + how much to pre-warm, bounded by disk / CPU / pod density. The math is in
``plan_benchmark`` (pure, unit-tested); ``probe_capacity`` reads the live cluster.

Default is **plan-only** (no cluster mutation). Pass ``--execute`` to actually run.

Example (plan only, read-only — recommended first):
    python -m tests.run_full_swebench_benchmark \\
        --context gke_PROJ_REGION_CLUSTER --namespace agent-sandbox-rl \\
        --node-selector cloud.google.com/gke-nodepool=gvisor-pool-500 \\
        --n-images 500 --avg-image-gb 10

Then, to actually run it:
    python -m tests.run_full_swebench_benchmark ... --execute --limit 500
"""

from __future__ import annotations

import argparse
import json
import os
import time

from agent_sandbox_rl import (BenchmarkPlan, ClusterCapacity, ClusterConfig,
                              FleetConfig, SandboxFleet, SweBenchSource, TemplateSpec,
                              make_rewriter, plan_benchmark, probe_capacity,
                              swebench_probe)
from agent_sandbox_rl import strategies

import loadtest  # sibling helpers: stage_metrics, format_report, _GLOSSARY, parse_node_selector

GVISOR_POOL_DEFAULT = "cloud.google.com/gke-nodepool=gvisor-pool-500"


# The capacity probe + planner live in the package now (agent_sandbox_rl.capacity):
# `probe_capacity`, `plan_benchmark`, `ClusterCapacity`, `BenchmarkPlan`. This module is
# the live runner (build a fleet from the plan, time PRELOAD vs TASK, emit the report).
# --------------------------------------------------------------------------- #
# Report
# --------------------------------------------------------------------------- #
def format_report(params: dict, cap: ClusterCapacity, plan: BenchmarkPlan,
                  result: dict | None) -> str:
    L = ["# Full SWE-bench benchmark — capacity-aware preload plan\n"]
    L.append("## Parameters\n")
    for k in ("n_images", "tasks_per_image", "total_tasks", "avg_image_gb",
              "cpu_request_milli", "disk_headroom", "executed", "image_source"):
        if k in params:
            L.append(f"- **{k}**: `{params[k]}`")
    L.append("")

    L.append("## Cluster capacity (probed)\n")
    L.append(f"- **pool**: `{cap.pool}`  **nodes**: {cap.nodes}  "
             f"**machine**: {', '.join(cap.machine_types) or 'n/a'}")
    L.append(f"- **CPU**: {cap.cpu_milli_total/1000:.0f} vCPU total "
             f"(~{cap.cpu_milli_per_node/1000:.1f}/node)")
    L.append(f"- **disk (allocatable ephemeral)**: {cap.disk_gb_total:.0f} GiB total "
             f"(~{cap.disk_gb_per_node:.0f} GiB/node)")
    L.append(f"- **pods**: {cap.pods_total} total (~{cap.pods_per_node}/node)")
    L.append("")

    L.append("## Recommended plan\n")
    L.append("| field | value |")
    L.append("|---|---|")
    L.append(f"| strategy | **{plan.strategy}** |")
    L.append(f"| max_concurrent | **{plan.max_concurrent}** |")
    L.append(f"| window_size | {plan.window_size if plan.window_size is not None else 'all (none)'} |")
    L.append(f"| replicas/image | {plan.replicas_per_image} |")
    L.append(f"| warm_per_task | {plan.warm_per_task} |")
    L.append(f"| colocate_replicas | {plan.colocate} |")
    L.append(f"| warm pods ({'peak' if plan.strategy != 'naive' else 'all'}) | {plan.total_warm_pods} |")
    L.append(f"| resident disk/node | {plan.resident_disk_per_node_gb:.0f} / "
             f"{plan.usable_disk_per_node_gb:.0f} GiB usable |")
    L.append(f"| bottleneck | {plan.bottleneck} |")
    L.append("")
    L.append("**Why:**")
    for r in plan.rationale:
        L.append(f"- {r}")
    L.append("")

    if result is None:
        L.append("> _Plan only — no pools were created. Re-run with `--execute` to "
                 "preload + run and fill in the timings below._\n")
        return "\n".join(L)

    m = loadtest.stage_metrics(result)
    L.append("## Results — preload vs task\n")
    L.append("> **PRELOAD** = pull + uncompress every image and bring sandboxes Ready "
             "(`create_warmpool` + `wait_pool_ready`); **TASK** = claim a ready sandbox "
             "+ run the probe (`claim` + `process`). Both are wall-clock.\n")
    L.append("| phase | wall | per-stage detail |")
    L.append("|---|---:|---|")
    L.append(f"| **PRELOAD** (pull+uncompress+ready) | {result['preload_wall_s']:.1f}s | "
             f"pool-ready avg/max {m['wait_avg_s']:.1f}/{m['wait_max_s']:.0f}s, "
             f"{m['warm_pools_created']} pools |")
    L.append(f"| **TASK** (claim+run) | {result['task_wall_s']:.1f}s | "
             f"claim avg/max {m['claim_avg_s']:.1f}/{m['claim_max_s']:.0f}s, "
             f"net task avg {m['net_task_avg_s']:.2f}s |")
    L.append(f"| **TOTAL** | {result['wall_s']:.1f}s | "
             f"{m['ok']}/{m['total']} tasks ok, warm peak {m['warm_peak']} |")
    L.append("")
    L.append("## Metric glossary\n")
    L.append(loadtest._GLOSSARY)
    L.append("")
    L.append("## Full RunReport\n```")
    rep = result["report"]
    if rep.get("error"):
        L.append(f"  ERROR: {rep['error']}")
    for ph, c in rep.get("phases", {}).items():
        L.append(f"  {ph:<16} {c['total_s']:8.2f}s  (n={c['count']}, max={c['max_s']:.2f}s)")
    L.append(f"  {'TOTAL wall':<16} {result['wall_s']:8.2f}s")
    L.append(f"  claims={rep.get('claims')}  tasks={rep.get('tasks_ok')}ok/"
             f"{rep.get('tasks_err')}err  warm peak={rep.get('warm_replicas_peak')}")
    L.append("```")
    return "\n".join(L)


# --------------------------------------------------------------------------- #
# Runner (needs a cluster)
# --------------------------------------------------------------------------- #
def _make_fleet(args, plan: BenchmarkPlan, cap: ClusterCapacity) -> SandboxFleet:
    sel = loadtest.parse_node_selector([args.node_selector] if args.node_selector else None)
    cluster = ClusterConfig(name="benchmark", context=args.context,
                            namespace=args.namespace, node_selector=sel,
                            runtime_class=args.runtime_class)
    cfg = FleetConfig(
        clusters=[cluster],
        max_concurrent=plan.max_concurrent,
        max_warmpool_size=args.max_warmpool_size,
        warm_per_task=plan.warm_per_task,
        window_size=plan.window_size,
        ready_timeout=args.ready_timeout,
        avg_image_gb=args.avg_image_gb,
        node_ephemeral_gb=cap.disk_gb_per_node,
        cluster_nodes=cap.nodes,                 # disk sizing spans the whole pool
        template=TemplateSpec(runtime_class=args.runtime_class,
                              colocate_replicas=plan.colocate),
    )
    return SandboxFleet(cfg)


def run_benchmark(args, plan: BenchmarkPlan, cap: ClusterCapacity) -> dict:
    """Execute the plan: timed PRELOAD (warm all/window) then timed TASK phase."""
    if args.tasks_per_image != 1:
        # The SWE-bench source yields one task per image, so RL-style sizing would
        # be planned (and reported) but never actually executed here. Use the
        # general load-test harness for repeated-tasks-per-image batches.
        raise ValueError(
            "--tasks-per-image > 1 is not supported by the SWE-bench runner "
            "(it executes one task per image); use tests/loadtest.py for "
            "RL-style batches.")
    fleet = _make_fleet(args, plan, cap)
    rewrite = (make_rewriter(registry=args.registry, project=args.registry_project,
                             repo=args.registry_repo)
               if args.registry else None)
    limit = args.limit if args.limit is not None else args.n_images
    fleet.load_tasks(SweBenchSource(limit=limit), image_rewrite=rewrite)
    probe = (lambda task, handle: handle.pod_name) if args.lightweight_probe else swebench_probe

    # Drive the primitives for an explicit PRELOAD vs TASK wall split, but inside the
    # observer's run() context so the RunReport records every phase (create_warmpool,
    # wait_pool_ready, claim, process, …) — those only accumulate within this context.
    preload_wall = task_wall = 0.0
    ok = 0
    with fleet.observer.run(plan.strategy) as report:
        fleet.report = report
        try:
            report.environment = fleet.describe_environment()
        except Exception:                          # noqa: BLE001 — best-effort
            pass
        t0 = time.monotonic()
        try:
            fleet.setup()                          # PRELOAD: preflight + plan + warm all (wait)
            preload_wall = time.monotonic() - t0
            t1 = time.monotonic()
            res = strategies.process_parallel(fleet, fleet.tasks, probe, plan.max_concurrent)
            task_wall = time.monotonic() - t1
            ok = sum(1 for r in res if not isinstance(r, Exception))
        finally:
            fleet.teardown()
    return {"strategy": plan.strategy, "wall_s": round(preload_wall + task_wall, 2),
            "preload_wall_s": round(preload_wall, 2), "task_wall_s": round(task_wall, 2),
            "ok": ok, "total": len(fleet.tasks), "report": report.to_dict()}


def _write(out: str, md: str, params: dict, cap: ClusterCapacity,
           plan: BenchmarkPlan, result: dict | None) -> None:
    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    with open(out, "w") as fh:
        fh.write(md)
    with open(out.rsplit(".", 1)[0] + ".json", "w") as fh:
        json.dump({"params": params, "capacity": cap.to_dict(),
                   "plan": plan.to_dict(), "result": result}, fh, indent=2)


def main(argv=None):
    args = build_parser().parse_args(argv)
    # Probe the live cluster via a Cluster's CoreV1Api (read-only).
    from agent_sandbox_rl.cluster import Cluster
    cluster = Cluster(ClusterConfig(name="probe", context=args.context,
                                    namespace=args.namespace))
    cap = probe_capacity(cluster.core_api, node_selector=args.node_selector)
    plan = plan_benchmark(cap, args.n_images, args.tasks_per_image,
                          avg_image_gb=args.avg_image_gb,
                          cpu_request_milli=args.cpu_request,
                          disk_headroom=args.disk_headroom,
                          max_pool=args.max_warmpool_size)
    params = {"n_images": args.n_images, "tasks_per_image": args.tasks_per_image,
              "total_tasks": plan.n_tasks, "avg_image_gb": args.avg_image_gb,
              "cpu_request_milli": args.cpu_request, "disk_headroom": args.disk_headroom,
              "executed": args.execute,
              "image_source": ("registry mirror" if args.registry else "SWE-bench (Docker Hub)")}

    result = None
    if args.execute:
        print(f"executing: {plan.strategy}, max_concurrent={plan.max_concurrent}, "
              f"limit={args.limit} ...", flush=True)
        result = run_benchmark(args, plan, cap)
        print(f"  preload={result['preload_wall_s']:.1f}s task={result['task_wall_s']:.1f}s "
              f"ok={result['ok']}/{result['total']}", flush=True)

    md = format_report(params, cap, plan, result)
    _write(args.out, md, params, cap, plan, result)
    print(f"\nwrote {args.out}\n", flush=True)
    print(md.split("## Recommended plan")[1].split("## Metric glossary")[0]
          if "## Recommended plan" in md else md, flush=True)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="Capacity-aware full SWE-bench preload planner.")
    p.add_argument("--n-images", type=int, default=500, help="distinct images (SWE-bench=500)")
    p.add_argument("--tasks-per-image", type=int, default=1, help=">1 = RL rollout shape")
    p.add_argument("--avg-image-gb", type=float, default=10.0,
                   help="avg uncompressed image size (GiB) for the disk-fit decision")
    p.add_argument("--cpu-request", type=int, default=250, help="per-pod CPU request (millicores)")
    p.add_argument("--disk-headroom", type=float, default=0.25)
    p.add_argument("--max-warmpool-size", type=int, default=64)
    p.add_argument("--node-selector", default=GVISOR_POOL_DEFAULT,
                   help="key=value label selector for the target node pool")
    p.add_argument("--context", default=None, help="kube context (default: ambient)")
    p.add_argument("--namespace", default="default")
    p.add_argument("--runtime-class", default="gvisor")
    p.add_argument("--ready-timeout", type=int, default=1800)
    # execution
    p.add_argument("--execute", action="store_true", help="actually preload + run (else plan only)")
    p.add_argument("--limit", type=int, default=None, help="tasks to run when executing (default: n-images)")
    p.add_argument("--lightweight-probe", action="store_true",
                   help="use a no-op probe instead of swebench_probe (measure infra only)")
    # optional in-region mirror rewrite
    p.add_argument("--registry", default=None, help="e.g. us-docker.pkg.dev")
    p.add_argument("--registry-project", default=None)
    p.add_argument("--registry-repo", default=None)
    p.add_argument("--out", default="performance_reports/full_swebench_plan.md")
    return p


if __name__ == "__main__":
    main()
