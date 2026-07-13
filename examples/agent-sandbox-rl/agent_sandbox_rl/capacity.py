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

"""Capacity-aware preload planning.

Probe a node pool's allocatable CPU / ephemeral disk / pod density, then compute the
optimal preload plan for an *N*-image (x *K*-tasks) batch — strategy (``naive`` warm-all
when it fits, else a disk-bounded ``pipelined`` window), ``max_concurrent``, per-image
replicas, and the binding bottleneck — so every image is pulled + uncompressed and warm
*before* the task phase starts.

Public API (also re-exported from ``agent_sandbox_rl``)::

    from agent_sandbox_rl import probe_capacity, plan_benchmark, render_plan
    cap  = probe_capacity(cluster.core_api, "cloud.google.com/gke-nodepool=mypool")
    plan = plan_benchmark(cap, n_images=500, tasks_per_image=1)
    print(render_plan(cap, plan))

Pure (cluster-free) except ``probe_capacity``, which only reads ``core_api.list_node``.
"""

from __future__ import annotations

import dataclasses
import math
from collections import OrderedDict

from . import sizing

GB = 1024 ** 3  # treat "GB" as GiB throughout (matches k8s allocatable units)

_BYTE_UNITS = {
    "Ki": 1024, "Mi": 1024 ** 2, "Gi": 1024 ** 3, "Ti": 1024 ** 4,
    "Pi": 1024 ** 5, "Ei": 1024 ** 6,
    "k": 1000, "K": 1000, "M": 1000 ** 2, "G": 1000 ** 3,
    "T": 1000 ** 4, "P": 1000 ** 5, "E": 1000 ** 6,
}


# --------------------------------------------------------------------------- #
# Quantity parsing (pure)
# --------------------------------------------------------------------------- #
def parse_cpu_milli(q) -> int:
    """k8s CPU quantity -> millicores. ``"31850m"`` -> 31850, ``"16"`` -> 16000."""
    s = str(q).strip()
    if s.endswith("m"):
        return int(float(s[:-1]))
    return int(float(s) * 1000)


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
    ``node_selector`` (a ``key=value`` label selector, or None for all nodes).

    ``core_api`` is a kubernetes ``CoreV1Api`` (e.g. ``Cluster.core_api``)."""
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
# The planner (pure)
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
    if cpu_request_milli < 1:
        raise ValueError("cpu_request_milli must be >= 1")
    if max_pool < 1:
        raise ValueError("max_pool must be >= 1")
    if avg_image_gb <= 0:
        raise ValueError("avg_image_gb must be > 0")
    if not 0.0 <= disk_headroom < 1.0:
        raise ValueError("disk_headroom must be in [0, 1)")
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
        # Per-node resident: the window's distinct images spread across the pool,
        # x2 for the double-buffered (up to 2-window) pipeline footprint. Mirrors
        # the naive branch's per-node accounting so the rendered disk/node is honest.
        window_per_node = math.ceil(window_size / nodes)
        resident = window_per_node * replicas * avg_image_gb * 2
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
        # Peak = up to 2 windows resident (double-buffered: current + prefetch),
        # matching the x2 per-node disk estimate above — not just one window.
        total_warm_pods = window_size * replicas * 2

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
# Rendering (no external deps — usable anywhere)
# --------------------------------------------------------------------------- #
def render_plan(cap: ClusterCapacity, plan: BenchmarkPlan) -> str:
    """Human-readable summary of the probed capacity + recommended plan."""
    win = plan.window_size if plan.window_size is not None else "all (none)"
    lines = [
        "Cluster capacity",
        f"  pool          : {cap.pool}",
        f"  nodes         : {cap.nodes}  ({', '.join(cap.machine_types) or 'n/a'})",
        f"  CPU           : {cap.cpu_milli_total/1000:.0f} vCPU "
        f"(~{cap.cpu_milli_per_node/1000:.1f}/node)",
        f"  disk (alloc)  : {cap.disk_gb_total:.0f} GiB (~{cap.disk_gb_per_node:.0f}/node)",
        f"  pods          : {cap.pods_total} (~{cap.pods_per_node}/node)",
        "",
        f"Recommended plan for {plan.n_images} images x {plan.tasks_per_image} "
        f"= {plan.n_tasks} tasks",
        f"  strategy          : {plan.strategy}",
        f"  max_concurrent    : {plan.max_concurrent}",
        f"  window_size       : {win}",
        f"  replicas/image    : {plan.replicas_per_image}",
        f"  warm_per_task     : {plan.warm_per_task}",
        f"  colocate_replicas : {plan.colocate}",
        f"  warm pods         : {plan.total_warm_pods}"
        f" ({'peak' if plan.strategy != 'naive' else 'all'})",
        f"  resident disk/node: {plan.resident_disk_per_node_gb:.0f} / "
        f"{plan.usable_disk_per_node_gb:.0f} GiB usable",
        f"  bottleneck        : {plan.bottleneck}",
        "",
        "Why:",
    ]
    lines += [f"  - {r}" for r in plan.rationale]
    return "\n".join(lines)
