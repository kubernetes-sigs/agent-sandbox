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

"""Unit tests for the capacity planner (agent_sandbox_rl.capacity)."""

from types import SimpleNamespace

import pytest

from agent_sandbox_rl import capacity

GiB = 1024 ** 3


# --- quantity parsing ------------------------------------------------------ #
def test_parse_cpu_milli():
  assert capacity.parse_cpu_milli("31850m") == 31850
  assert capacity.parse_cpu_milli("16") == 16000
  assert capacity.parse_cpu_milli("1500m") == 1500
  assert capacity.parse_cpu_milli("0") == 0


def test_parse_quantity_bytes():
  assert capacity.parse_quantity_bytes("364209683290") == 364209683290    # plain bytes
  assert capacity.parse_quantity_bytes("339Gi") == int(339 * GiB)
  assert capacity.parse_quantity_bytes("1000Ki") == 1000 * 1024           # binary before 'K'
  assert capacity.parse_quantity_bytes("1G") == 1000 ** 3                 # decimal
  assert capacity.parse_quantity_bytes("110") == 110


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
  cap = capacity.probe_capacity(core, node_selector="cloud.google.com/gke-nodepool=gvisor-pool-500")
  assert cap.nodes == 2
  assert cap.cpu_milli_total == 2 * 31850
  assert cap.pods_total == 220
  assert cap.machine_types == ["e2-standard-32"]
  assert cap.pool == "gvisor-pool-500"
  assert cap.disk_gb_per_node == pytest.approx(364209683290 / GiB, rel=1e-3)


def test_probe_capacity_raises_on_no_nodes():
  core = SimpleNamespace(list_node=lambda label_selector=None: SimpleNamespace(items=[]))
  with pytest.raises(ValueError):
    capacity.probe_capacity(core, node_selector="nope=nope")


# --- the planner ----------------------------------------------------------- #
def _cap(nodes=30, vcpu=31.85, disk_gib=339.2, pods=110, pool="gvisor-pool-500"):
  return capacity.ClusterCapacity(
      pool=pool, nodes=nodes, machine_types=["e2-standard-32"],
      cpu_milli_total=int(nodes * vcpu * 1000),
      disk_gb_total=round(nodes * disk_gib, 1), pods_total=nodes * pods)


def test_plan_big_cluster_fits_all_naive():
  plan = capacity.plan_benchmark(_cap(), n_images=500, tasks_per_image=1, avg_image_gb=10)
  assert plan.strategy == "naive"
  assert plan.window_size is None
  assert plan.max_concurrent == 500          # all tasks at once
  assert plan.replicas_per_image == 1
  assert plan.total_warm_pods == 500
  assert plan.bottleneck == "none"
  assert plan.resident_disk_per_node_gb <= plan.usable_disk_per_node_gb


def test_plan_disk_bound_falls_back_to_pipelined():
  cap = _cap(nodes=3, disk_gib=50)            # tiny disk -> can't warm 500 -> pipelined
  plan = capacity.plan_benchmark(cap, n_images=500, tasks_per_image=1, avg_image_gb=10)
  assert plan.strategy == "pipelined"
  assert plan.bottleneck == "disk"
  assert plan.window_size is not None and plan.window_size >= 1
  assert plan.max_concurrent >= 1


def test_pipelined_resident_disk_is_per_node_not_cluster_total():
  # Disk-bound -> pipelined. The reported resident disk must be a PER-NODE figure
  # (window spread across the pool, x2 for double-buffering), not a cluster total,
  # so it stays consistent with the planner's own per-node fit budget.
  cap = _cap(nodes=30, disk_gib=50)           # tiny disk/node -> pipelined
  plan = capacity.plan_benchmark(cap, n_images=500, tasks_per_image=1, avg_image_gb=10)
  assert plan.strategy == "pipelined"
  import math
  nodes = max(1, cap.nodes)
  expected = math.ceil(plan.window_size / nodes) * plan.replicas_per_image * 10 * 2
  assert plan.resident_disk_per_node_gb == round(expected, 1)
  # the old (buggy) cluster-wide figure would be window*replicas*gb — far larger
  cluster_total = plan.window_size * plan.replicas_per_image * 10
  assert plan.resident_disk_per_node_gb < cluster_total
  # peak warm pods = up to 2 windows (double-buffered), not one
  assert plan.total_warm_pods == plan.window_size * plan.replicas_per_image * 2


def test_plan_cpu_bound_falls_back_to_pipelined():
  cap = _cap(nodes=30, vcpu=4, disk_gib=1000)  # 120 vCPU total < 500*0.25
  plan = capacity.plan_benchmark(cap, n_images=500, tasks_per_image=1,
                                 avg_image_gb=1, cpu_request_milli=250)
  assert plan.strategy == "pipelined"
  assert plan.bottleneck == "cpu"
  assert plan.max_concurrent <= 120_000 // 250


def test_plan_rl_shape_enables_instant_claim():
  plan = capacity.plan_benchmark(_cap(), n_images=50, tasks_per_image=8, avg_image_gb=10)
  assert plan.warm_per_task is True
  assert plan.colocate is True
  assert plan.replicas_per_image == 8
  assert plan.n_tasks == 400


def test_plan_rejects_bad_counts():
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=0)
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=10, tasks_per_image=0)


def test_plan_rejects_bad_numeric_args():
  # division/headroom inputs must be validated, not silently div-by-zero or go negative
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=10, cpu_request_milli=0)
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=10, max_pool=0)
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=10, avg_image_gb=0)
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=10, disk_headroom=1.0)
  with pytest.raises(ValueError):
    capacity.plan_benchmark(_cap(), n_images=10, disk_headroom=-0.1)


def test_plan_guards_zero_nodes():
  # plan_benchmark is independently callable; a 0-node snapshot must not div-by-zero
  cap = capacity.ClusterCapacity(pool="empty", nodes=0, machine_types=[],
                                 cpu_milli_total=0, disk_gb_total=0.0, pods_total=0)
  plan = capacity.plan_benchmark(cap, n_images=10, tasks_per_image=1)
  assert plan.max_concurrent >= 1


# --- rendering ------------------------------------------------------------- #
def test_render_plan_includes_capacity_and_recommendation():
  cap = _cap()
  plan = capacity.plan_benchmark(cap, 500, 1, avg_image_gb=10)
  out = capacity.render_plan(cap, plan)
  assert "Cluster capacity" in out
  assert "Recommended plan" in out
  assert "strategy" in out and "naive" in out
  assert "max_concurrent" in out
  assert "Why:" in out
