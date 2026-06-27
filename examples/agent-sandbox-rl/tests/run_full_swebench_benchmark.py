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
import dataclasses
import json
import math
import os
import time
from collections import OrderedDict

from agent_sandbox_rl import (ClusterConfig, FleetConfig, SandboxFleet,
                              SweBenchSource, TemplateSpec, make_rewriter,
                              swebench_probe)
from agent_sandbox_rl import sizing, strategies

import loadtest  # sibling helpers: stage_metrics, format_report, _GLOSSARY, parse_node_selector

GVISOR_POOL_DEFAULT = "cloud.google.com/gke-nodepool=gvisor-pool-500"
GB = 1024 ** 3  # treat "GB" as GiB throughout (matches k8s allocatable units)


# --------------------------------------------------------------------------- #
# Pure helpers — quantity parsing (unit-tested, no cluster)
# --------------------------------------------------------------------------- #
def parse_cpu_milli(q) -> int:
    """k8s CPU quantity -> millicores. ``"31850m"`` -> 31850, ``"16"`` -> 16000."""
    s = str(q).strip()
    if s.endswith("m"):
        return int(float(s[:-1]))
    return int(float(s) * 1000)


_BYTE_UNITS = {
    "Ki": 1024, "Mi": 1024 ** 2, "Gi": 1024 ** 3, "Ti": 1024 ** 4,
    "Pi": 1024 ** 5, "Ei": 1024 ** 6,
    "k": 1000, "K": 1000, "M": 1000 ** 2, "G": 1000 ** 3,
    "T": 1000 ** 4, "P": 1000 ** 5, "E": 1000 ** 6,
}


def parse_quantity_bytes(q) -> int:
    """k8s storage/memory quantity -> bytes. Handles ``"339Gi"``, ``"1000Ki"``,
    plain ``"364209683290"``. (Binary suffixes checked before single-letter ones.)"""
    s = str(q).strip()
    for suf, mult in sorted(_BYTE_UNITS.items(), key=lambda kv: -len(kv[0])):
        if s.endswith(suf):
            return int(float(s[:-len(suf)]) * mult)
    return int(float(s))


# --------------------------------------------------------------------------- #
# Cluster capacity
# --------------------------------------------------------------------------- #
@dataclasses.dataclass
class ClusterCapacity:
    pool: str
    nodes: int
    machine_types: list[str]
    cpu_milli_total: int
    disk_gb_total: float           # allocatable ephemeral storage, GiB
    pods_total: int

    @property
    def cpu_milli_per_node(self) -> int:
        return self.cpu_milli_total // max(1, self.nodes)

    @property
    def disk_gb_per_node(self) -> float:
        return self.disk_gb_total / max(1, self.nodes)

    @property
    def pods_per_node(self) -> int:
        return self.pods_total // max(1, self.nodes)

    def to_dict(self) -> dict:
        d = dataclasses.asdict(self)
        d.update(cpu_milli_per_node=self.cpu_milli_per_node,
                 disk_gb_per_node=round(self.disk_gb_per_node, 1),
                 pods_per_node=self.pods_per_node,
                 vcpu_total=round(self.cpu_milli_total / 1000, 1))
        return d


def probe_capacity(core_api, node_selector: str | None = None,
                   pool_label: str = "cloud.google.com/gke-nodepool") -> ClusterCapacity:
    """Sum allocatable cpu / ephemeral-storage / pods over the nodes matching
    ``node_selector`` (a ``key=value`` label selector, or None for all nodes)."""
    nodes = (core_api.list_node(label_selector=node_selector)
             if node_selector else core_api.list_node()).items
    if not nodes:
        raise ValueError(f"no nodes match selector {node_selector!r}")
    cpu = 0
    disk_gb = 0.0
    pods = 0
    machines: set[str] = set()
    pools: set[str] = set()
    for n in nodes:
        alloc = (n.status.allocatable or {}) if n.status else {}
        cpu += parse_cpu_milli(alloc.get("cpu", "0"))
        disk_gb += parse_quantity_bytes(alloc.get("ephemeral-storage", "0")) / GB
        pods += int(float(alloc.get("pods", "0")))
        labels = (n.metadata.labels or {}) if n.metadata else {}
        mt = labels.get("node.kubernetes.io/instance-type")
        if mt:
            machines.add(mt)
        p = labels.get(pool_label)
        if p:
            pools.add(p)
    return ClusterCapacity(
        pool=", ".join(sorted(pools)) or "(unlabeled)",
        nodes=len(nodes),
        machine_types=sorted(machines),
        cpu_milli_total=cpu,
        disk_gb_total=round(disk_gb, 1),
        pods_total=pods,
    )


# --------------------------------------------------------------------------- #
# The planner (pure — the heart, unit-tested)
# --------------------------------------------------------------------------- #
@dataclasses.dataclass
class BenchmarkPlan:
    strategy: str
    max_concurrent: int
    window_size: int | None
    warm_per_task: bool
    colocate: bool
    replicas_per_image: int
    total_warm_pods: int
    resident_disk_per_node_gb: float
    usable_disk_per_node_gb: float
    bottleneck: str               # none | cpu | pods | disk | tasks
    n_images: int
    tasks_per_image: int
    n_tasks: int
    rationale: list[str]

    def to_dict(self) -> dict:
        return dataclasses.asdict(self)


def plan_benchmark(cap: ClusterCapacity, n_images: int, tasks_per_image: int = 1, *,
                   avg_image_gb: float = 10.0, cpu_request_milli: int = 250,
                   disk_headroom: float = 0.25, max_pool: int = 64) -> BenchmarkPlan:
    """Compute the optimal preload plan for ``n_images`` images (``tasks_per_image``
    tasks each) given the cluster's CPU / disk / pod capacity.

    Strategy: if every image's warm replicas fit resident on disk *and* the full warm
    set fits CPU + pod budgets, **warm everything up front** (``naive`` — the whole
    preload happens before tasks) at the highest safe concurrency. Otherwise fall back
    to a disk-bounded ``pipelined`` window that overlaps pulls with execution.
    """
    if n_images < 1 or tasks_per_image < 1:
        raise ValueError("n_images and tasks_per_image must be >= 1")
    n_tasks = n_images * tasks_per_image
    rl = tasks_per_image > 1                      # RL rollout shape -> instant-claim levers
    warm_per_task = rl
    colocate = rl
    replicas = min(tasks_per_image, max_pool) if rl else 1

    nodes = max(1, cap.nodes)                              # guard empty-pool snapshots
    cpu_cap = cap.cpu_milli_total // cpu_request_milli      # max concurrent pods by CPU
    pod_cap = cap.pods_total                                # max pods by density
    conc_cap = max(1, min(cpu_cap, pod_cap))               # task-phase concurrency ceiling
    usable_disk_per_node = (cap.disk_gb_total / nodes) * (1 - disk_headroom)

    # Footprint of warming EVERYTHING (distinct images spread across nodes).
    images_per_node = math.ceil(n_images / nodes)
    resident_per_node = images_per_node * replicas * avg_image_gb
    total_warm_pods = n_images * replicas
    disk_fits_all = resident_per_node <= usable_disk_per_node
    pods_fit_all = total_warm_pods <= pod_cap
    cpu_fits_all = total_warm_pods * cpu_request_milli <= cap.cpu_milli_total

    rationale: list[str] = [
        f"{cap.nodes} nodes x ~{cap.cpu_milli_per_node/1000:.1f} vCPU / "
        f"~{cap.disk_gb_per_node:.0f} GiB disk / {cap.pods_per_node} pods.",
        f"CPU budget: {cap.cpu_milli_total/1000:.0f} vCPU / {cpu_request_milli}m "
        f"per pod = {cpu_cap} concurrent pods.",
        f"Pod-density budget: {pod_cap} pods.",
        f"Usable disk/node (after {int(disk_headroom*100)}% headroom): "
        f"{usable_disk_per_node:.0f} GiB.",
    ]

    if disk_fits_all and pods_fit_all and cpu_fits_all:
        strategy = "naive"
        window_size = None
        max_concurrent = min(n_tasks, conc_cap)
        resident = resident_per_node
        if max_concurrent >= n_tasks:
            bottleneck = "none"
        else:
            bottleneck = "cpu" if cpu_cap <= pod_cap else "pods"
        rationale.append(
            f"All {total_warm_pods} warm pods fit (disk {resident_per_node:.0f} "
            f"<= {usable_disk_per_node:.0f} GiB/node, pods {total_warm_pods} <= {pod_cap}, "
            f"CPU {total_warm_pods*cpu_request_milli/1000:.0f} <= {cap.cpu_milli_total/1000:.0f} vCPU) "
            "-> preload EVERYTHING up front (naive).")
        rationale.append(
            f"Task concurrency = min(tasks={n_tasks}, conc_cap={conc_cap}) = {max_concurrent}"
            + ("" if bottleneck == "none" else f" (limited by {bottleneck})") + ".")
    else:
        strategy = "pipelined"
        totals = OrderedDict((f"img{i}", tasks_per_image) for i in range(n_images))
        window_size = sizing.recommend_window_pipelined(
            totals, conc_cap, max_pool,
            avg_image_gb=avg_image_gb, usable_disk_gb=usable_disk_per_node,
            per_task=warm_per_task, nodes=cap.nodes)
        max_concurrent = max(1, min(conc_cap, window_size * replicas))
        resident = window_size * replicas * avg_image_gb     # per "double-buffer" footprint
        bottleneck = ("disk" if not disk_fits_all
                      else "pods" if not pods_fit_all else "cpu")
        why = []
        if not disk_fits_all:
            why.append(f"disk ({resident_per_node:.0f} > {usable_disk_per_node:.0f} GiB/node)")
        if not pods_fit_all:
            why.append(f"pods ({total_warm_pods} > {pod_cap})")
        if not cpu_fits_all:
            why.append(f"CPU ({total_warm_pods*cpu_request_milli/1000:.0f} > {cap.cpu_milli_total/1000:.0f} vCPU)")
        rationale.append("Cannot warm everything (" + ", ".join(why)
                         + ") -> pipelined, overlap pulls with execution.")
        rationale.append(
            f"Disk-bounded window = {window_size} image(s) resident; "
            f"task concurrency = {max_concurrent}.")
        total_warm_pods = window_size * replicas              # peak, not all-at-once

    if rl:
        rationale.append(
            f"{tasks_per_image} tasks/image -> warm_per_task + colocate_replicas "
            f"({replicas} replicas/image, instant claims).")

    return BenchmarkPlan(
        strategy=strategy, max_concurrent=max_concurrent, window_size=window_size,
        warm_per_task=warm_per_task, colocate=colocate, replicas_per_image=replicas,
        total_warm_pods=total_warm_pods,
        resident_disk_per_node_gb=round(resident, 1),
        usable_disk_per_node_gb=round(usable_disk_per_node, 1),
        bottleneck=bottleneck, n_images=n_images, tasks_per_image=tasks_per_image,
        n_tasks=n_tasks, rationale=rationale)


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
