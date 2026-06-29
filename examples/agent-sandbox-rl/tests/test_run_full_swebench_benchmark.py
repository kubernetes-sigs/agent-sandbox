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

"""CLI-glue tests for the full-SWE-bench runner. The planner itself is covered by
``test_capacity.py``; here we test the runner's preload/task report rendering."""

import types

import pytest

import run_full_swebench_benchmark as bench

GiB = 1024 ** 3


def _cap(nodes=30, vcpu=31.85, disk_gib=339.2, pods=110):
  return bench.ClusterCapacity(
      pool="gvisor-pool-500", nodes=nodes, machine_types=["e2-standard-32"],
      cpu_milli_total=int(nodes * vcpu * 1000),
      disk_gb_total=round(nodes * disk_gib, 1), pods_total=nodes * pods)


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


def test_run_benchmark_rejects_tasks_per_image_gt_1():
  # The SWE-bench source is 1 task/image, so RL-style sizing would be planned but
  # never executed — the runner must refuse rather than silently mislead.
  cap = _cap()
  plan = bench.plan_benchmark(cap, 50, 8, avg_image_gb=10)
  args = types.SimpleNamespace(tasks_per_image=8)
  with pytest.raises(ValueError, match="tasks-per-image"):
    bench.run_benchmark(args, plan, cap)
