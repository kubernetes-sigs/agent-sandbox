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

"""Unit tests for the pure helpers of the (cluster-driven) load test harness."""

import pytest

import loadtest


def test_build_tasks_shape_and_distinct_images():
  tasks = loadtest.build_tasks(5, 3, "reg/repo/bench:img{i:03d}")
  assert len(tasks) == 15                       # 5 images x 3 tasks
  assert len({t.image for t in tasks}) == 5     # distinct images
  assert tasks[0].image == "reg/repo/bench:img000"
  assert tasks[-1].image == "reg/repo/bench:img004"
  assert len({t.id for t in tasks}) == 15       # unique task ids


def test_build_tasks_rejects_bad_counts():
  with pytest.raises(ValueError):
    loadtest.build_tasks(0, 1, "x:{i}")
  with pytest.raises(ValueError):
    loadtest.build_tasks(1, 0, "x:{i}")


def test_build_tasks_rejects_non_varying_template():
  # a template without {i} would silently produce N identical images
  with pytest.raises(ValueError):
    loadtest.build_tasks(5, 1, "reg/repo/bench:latest")
  # single image is fine even without {i}
  assert len(loadtest.build_tasks(1, 2, "reg/repo/bench:latest")) == 2


def test_parser_exposes_instant_claim_flags():
  p = loadtest.build_parser()
  # defaults off
  a = p.parse_args(["--images", "2", "--image-template", "r/b:img{i}"])
  assert a.warm_per_task is False and a.colocate is False
  assert a.max_warmpool_size == 8
  # opt-in
  a2 = p.parse_args(["--images", "2", "--image-template", "r/b:img{i}",
                     "--warm-per-task", "--colocate", "--max-warmpool-size", "16"])
  assert a2.warm_per_task is True and a2.colocate is True
  assert a2.max_warmpool_size == 16


def test_methodology_notes_instant_claim_mode():
  md = loadtest.format_report(
      {"images": 1, "tasks_per_image": 4, "total_tasks": 4, "strategies": "naive",
       "max_concurrent": 8, "max_warmpool_size": 16, "warm_per_task": True,
       "colocate_replicas": True, "window_size": None, "task_duration_s": 0.0,
       "image_template": "r/b:img{i}"},
      [], {}, [_fake_result("naive", 4.0)], ["r/b:img0"])
  assert "RL instant-claim mode" in md
  assert "per-task sizing" in md and "replica co-location" in md
  assert "warm_per_task" in md


def test_parse_node_selector():
  assert loadtest.parse_node_selector(None) is None
  assert loadtest.parse_node_selector(
      ["cloud.google.com/gke-nodepool=pool-fat"]) == {
      "cloud.google.com/gke-nodepool": "pool-fat"}
  with pytest.raises(ValueError):
    loadtest.parse_node_selector(["nope"])


def test_resolve_strategies():
  assert loadtest.resolve_strategies("all") == ["none", "naive", "sliding", "pipelined"]
  assert loadtest.resolve_strategies("sliding, pipelined") == ["sliding", "pipelined"]
  with pytest.raises(ValueError):
    loadtest.resolve_strategies("bogus")


def _fake_result(strategy, wall, ok=10, total=10):
  return {
      "strategy": strategy, "wall_s": wall, "ok": ok, "total": total,
      "report": {
          "phases": {
              "create_warmpool": {"count": 5, "total_s": 2.0, "max_s": 0.5},
              "wait_pool_ready": {"count": 5, "total_s": 50.0, "max_s": 20.0},
              "claim": {"count": 10, "total_s": 8.0, "max_s": 1.0},
              "process": {"count": 10, "total_s": 3.0, "max_s": 0.4},
          },
          "claims": 10, "tasks_ok": ok, "tasks_err": total - ok,
          "warm_replicas_peak": 5, "warm_replicas_total": 5,
          "environment": {"c": {"context": "ctx", "namespace": "ns",
                                "k8s_version": "v1.30", "nodes": 3,
                                "node_pools": ["p"], "instance_types": ["e2"],
                                "region": "us"}},
      },
  }


def test_stage_metrics_derivations():
  m = loadtest.stage_metrics(_fake_result("sliding", 12.0))
  assert m["wait_avg_s"] == pytest.approx(10.0)      # 50/5
  assert m["wait_max_s"] == 20.0
  assert m["claims"] == 10
  assert m["claim_avg_s"] == pytest.approx(0.8)        # 8/10 -> avg time-to-sandbox
  assert m["claim_max_s"] == 1.0                        # slowest single claim
  assert m["net_task_s"] == 3.0                        # process total = net task time
  assert m["net_task_avg_s"] == pytest.approx(0.3)
  assert m["warm_pools_created"] == 5
  assert m["warm_peak"] == 5


def test_format_report_renders_sections_and_efficiency():
  results = [_fake_result("none", 20.0), _fake_result("sliding", 10.0)]
  plan = [{"image": "reg/repo/bench:img000", "pool": "pool-x",
           "replicas": 1, "cluster": "loadtest"}]
  env = results[0]["report"]["environment"]
  params = {"images": 5, "tasks_per_image": 2, "total_tasks": 10,
            "strategies": "none,sliding", "max_concurrent": 40,
            "window_size": None, "task_duration_s": 0.0,
            "image_template": "reg/repo/bench:img{i:03d}"}
  md = loadtest.format_report(params, plan, env, results,
                              ["reg/repo/bench:img000"])
  assert "# SWE-bench load test" in md
  assert "## Methodology" in md
  assert "## Cluster & nodes" in md
  assert "## Metric glossary" in md
  assert "Warm-pool plan (1 pools)" in md
  assert "pool-x" in md
  # claim latency (time-to-sandbox) column + glossary entry are present
  assert "claim avg/max" in md
  assert "time-to-sandbox" in md
  # methodology reflects the params it was given
  assert "5** distinct container images" in md and "max_concurrent=40" in md
  # sliding (10s) is fastest -> 100%; none (20s) -> 50%
  assert "100%" in md and "50%" in md


def test_methodology_lists_only_requested_strategies():
  md = loadtest.format_report(
      {"images": 2, "tasks_per_image": 1, "total_tasks": 2,
       "strategies": "none,pipelined", "max_concurrent": 8,
       "window_size": None, "task_duration_s": 0.0,
       "image_template": "r/b:img{i}"},
      [], {}, [_fake_result("none", 5.0), _fake_result("pipelined", 4.0)],
      ["r/b:img0"])
  assert "- **none** —" in md and "- **pipelined** —" in md
  assert "- **naive** —" not in md          # not requested -> not described
