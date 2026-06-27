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

"""Unit tests for the pure helpers of the full-SWE-bench capacity planner."""

from types import SimpleNamespace

import pytest

import run_full_swebench_benchmark as bench

GiB = 1024 ** 3


# --- quantity parsing ------------------------------------------------------ #
def test_parse_cpu_milli():
  assert bench.parse_cpu_milli("31850m") == 31850
  assert bench.parse_cpu_milli("16") == 16000
  assert bench.parse_cpu_milli("1500m") == 1500
  assert bench.parse_cpu_milli("0") == 0


def test_parse_quantity_bytes():
  assert bench.parse_quantity_bytes("364209683290") == 364209683290     # plain bytes
  assert bench.parse_quantity_bytes("339Gi") == int(339 * GiB)
  assert bench.parse_quantity_bytes("1000Ki") == 1000 * 1024            # binary before 'K'
  assert bench.parse_quantity_bytes("1G") == 1000 ** 3                  # decimal
  assert bench.parse_quantity_bytes("110") == 110


# --- capacity probe (fake CoreV1Api) --------------------------------------- #
def _fake_node():
  return SimpleNamespace(
      status=SimpleNamespace(allocatable={
          "cpu": "31850m", "ephemeral-storage": "364209683290", "pods": "110"}),
      metadata=SimpleNamespace(labels={
          "node.kubernetes.io/instance-type": "e2-standard-32",
          "cloud.google.com/gke-nodepool": "gvisor-pool-500"}))


def test_probe_capacity_sums_allocatable():
  nodes = [_fake_node(), _fake_node()]
  core = SimpleNamespace(list_node=lambda label_selector=None: SimpleNamespace(items=nodes))
  cap = bench.probe_capacity(core, node_selector="cloud.google.com/gke-nodepool=gvisor-pool-500")
  assert cap.nodes == 2
  assert cap.cpu_milli_total == 2 * 31850
  assert cap.pods_total == 220
  assert cap.machine_types == ["e2-standard-32"]
  assert cap.pool == "gvisor-pool-500"
  assert cap.disk_gb_per_node == pytest.approx(364209683290 / GiB, rel=1e-3)


def test_probe_capacity_raises_on_no_nodes():
  core = SimpleNamespace(list_node=lambda label_selector=None: SimpleNamespace(items=[]))
  with pytest.raises(ValueError):
    bench.probe_capacity(core, node_selector="nope=nope")


# --- the planner ----------------------------------------------------------- #
def _cap(nodes=30, vcpu=31.85, disk_gib=339.2, pods=110, pool="gvisor-pool-500"):
  return bench.ClusterCapacity(
      pool=pool, nodes=nodes, machine_types=["e2-standard-32"],
      cpu_milli_total=int(nodes * vcpu * 1000),
      disk_gb_total=round(nodes * disk_gib, 1), pods_total=nodes * pods)


def test_plan_big_cluster_fits_all_naive():
  plan = bench.plan_benchmark(_cap(), n_images=500, tasks_per_image=1, avg_image_gb=10)
  assert plan.strategy == "naive"
  assert plan.window_size is None
  assert plan.max_concurrent == 500          # all tasks at once
  assert plan.replicas_per_image == 1
  assert plan.total_warm_pods == 500
  assert plan.bottleneck == "none"
  assert plan.resident_disk_per_node_gb <= plan.usable_disk_per_node_gb


def test_plan_disk_bound_falls_back_to_pipelined():
  # tiny disk per node -> can't warm 500 images -> pipelined, disk-bottlenecked
  cap = _cap(nodes=3, disk_gib=50)
  plan = bench.plan_benchmark(cap, n_images=500, tasks_per_image=1, avg_image_gb=10)
  assert plan.strategy == "pipelined"
  assert plan.bottleneck == "disk"
  assert plan.window_size is not None and plan.window_size >= 1
  assert plan.max_concurrent >= 1


def test_plan_cpu_bound_falls_back_to_pipelined():
  # plenty of disk/pods but too little CPU to warm all 500 pods at 250m
  cap = _cap(nodes=30, vcpu=4, disk_gib=1000)      # 120 vCPU total < 500*0.25
  plan = bench.plan_benchmark(cap, n_images=500, tasks_per_image=1,
                              avg_image_gb=1, cpu_request_milli=250)
  assert plan.strategy == "pipelined"
  assert plan.bottleneck == "cpu"
  assert plan.max_concurrent <= 120_000 // 250    # bounded by cpu_cap


def test_plan_rl_shape_enables_instant_claim():
  plan = bench.plan_benchmark(_cap(), n_images=50, tasks_per_image=8, avg_image_gb=10)
  assert plan.warm_per_task is True
  assert plan.colocate is True
  assert plan.replicas_per_image == 8
  assert plan.n_tasks == 400


def test_plan_rejects_bad_counts():
  with pytest.raises(ValueError):
    bench.plan_benchmark(_cap(), n_images=0)
  with pytest.raises(ValueError):
    bench.plan_benchmark(_cap(), n_images=10, tasks_per_image=0)


# --- report rendering ------------------------------------------------------ #
def test_format_report_plan_only():
  cap = _cap()
  plan = bench.plan_benchmark(cap, 500, 1, avg_image_gb=10)
  params = {"n_images": 500, "tasks_per_image": 1, "total_tasks": 500,
            "avg_image_gb": 10, "executed": False}
  md = bench.format_report(params, cap, plan, None)
  assert "# Full SWE-bench benchmark" in md
  assert "## Cluster capacity (probed)" in md
  assert "## Recommended plan" in md
  assert "naive" in md and "Plan only" in md


def test_format_report_with_result_shows_preload_vs_task():
  cap = _cap()
  plan = bench.plan_benchmark(cap, 500, 1, avg_image_gb=10)
  result = {
      "strategy": "naive", "wall_s": 400.0, "preload_wall_s": 350.0,
      "task_wall_s": 50.0, "ok": 500, "total": 500,
      "report": {"phases": {
          "create_warmpool": {"count": 500, "total_s": 100.0, "max_s": 1.0},
          "wait_pool_ready": {"count": 500, "total_s": 5000.0, "max_s": 60.0},
          "claim": {"count": 500, "total_s": 200.0, "max_s": 3.0},
          "process": {"count": 500, "total_s": 1500.0, "max_s": 5.0},
      }, "claims": 500, "tasks_ok": 500, "tasks_err": 0,
          "warm_replicas_peak": 500, "warm_replicas_total": 500}}
  params = {"n_images": 500, "tasks_per_image": 1, "total_tasks": 500, "executed": True}
  md = bench.format_report(params, cap, plan, result)
  assert "PRELOAD" in md and "TASK" in md
  assert "350.0s" in md and "50.0s" in md
  assert "## Metric glossary" in md
