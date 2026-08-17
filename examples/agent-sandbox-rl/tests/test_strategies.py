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

import time

import pytest

from agent_sandbox_rl import FleetConfig, SandboxFleet
from agent_sandbox_rl.preflight import PreflightReport


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _fleet(registry, **cfg):
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def test_naive_runs_all_and_tears_down(two_cluster_registry):
  f = _fleet(two_cluster_registry, placement="round-robin")
  f.load_tasks(["imgA", "imgB", "imgA"])
  seen = []
  res = f.run(lambda t, h: seen.append(t.image) or h.pod_name, strategy="naive")
  assert len(res) == 3
  assert sorted(seen) == ["imgA", "imgA", "imgB"]
  assert f.handles() == []
  for c in two_cluster_registry:
    assert c.active_claims == 0 and c.active_replicas == 0


def test_sliding_window_bounds_warm_images(make_cluster):
  # one cluster, 3 distinct images, window auto -> processes in batches.
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  reg = ClusterRegistry([c])
  f = _fleet(reg, placement="image-affinity", max_concurrent=1, window_size=1)
  f.load_tasks(["i1", "i2", "i3"])
  res = f.run(lambda t, h: t.id, strategy="sliding")
  assert len(res) == 3
  # window=1 -> at most one warmpool created+deleted per image (3 creates, 3 deletes)
  assert c.resources.create_warmpool.call_count == 3
  assert c.resources.delete_warmpool.call_count >= 3
  assert f.handles() == []


def test_none_forces_replicas_one(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  reg = ClusterRegistry([c])
  f = _fleet(reg, max_concurrent=4, max_warmpool_size=8)
  f.load_tasks(["i1", "i2"])
  f.run(lambda t, h: t.id, strategy="none")
  # every warmpool created with replicas == 1 regardless of budget
  for call in c.resources.create_warmpool.call_args_list:
    args = call.args
    assert args[2] == 1   # create_warmpool(name, template, replicas)


def test_naive_warms_pools_concurrently(make_cluster):
  # start_warmpools must warm pools in parallel: 10 pools each with a 0.1s
  # readiness wait should finish in ~0.1s, not ~1.0s sequential.
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")

  def _slow_ready(*a, **k):
    time.sleep(0.1)
    return True
  c.resources.wait_for_pool_ready.side_effect = _slow_ready

  f = _fleet(ClusterRegistry([c]), max_concurrent=10)
  f.load_tasks([f"img{i}" for i in range(10)])   # 10 distinct images -> 10 pools
  start = time.monotonic()
  f.run(lambda t, h: h.pod_name, strategy="naive", concurrency=10)
  elapsed = time.monotonic() - start
  assert elapsed < 0.6      # parallel warm; would be >=1.0s if serialized
  assert f.handles() == []


def test_sliding_warms_window_concurrently(make_cluster):
  # the windowed strategies must warm a window's images in PARALLEL too (not 1/s):
  # 10 images in one window, each 0.1s ready -> ~0.1s, not ~1.0s sequential.
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")

  def _slow_ready(*a, **k):
    time.sleep(0.1)
    return True
  c.resources.wait_for_pool_ready.side_effect = _slow_ready

  f = _fleet(ClusterRegistry([c]), max_concurrent=10)   # window auto -> 10 (all fit)
  f.load_tasks([f"img{i}" for i in range(10)])
  start = time.monotonic()
  res = f.run(lambda t, h: h.pod_name, strategy="sliding", concurrency=10)
  elapsed = time.monotonic() - start
  assert elapsed < 0.6      # parallel window warm; >=1.0s if serialized
  assert len(res) == 10 and f.handles() == []


def test_parallel_actually_overlaps(make_cluster):
  # With concurrency=4 and a 0.1s process, 4 tasks finish in ~0.1s not ~0.4s.
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  reg = ClusterRegistry([c])
  f = _fleet(reg, max_concurrent=4)
  f.load_tasks(["img"] * 4)   # same image -> one pool, 4 claims
  start = time.monotonic()
  f.run(lambda t, h: time.sleep(0.1), strategy="naive", concurrency=4)
  elapsed = time.monotonic() - start
  assert elapsed < 0.35      # would be >=0.4 if serial


def test_parallel_bookkeeping_is_consistent(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  reg = ClusterRegistry([c])
  f = _fleet(reg, max_concurrent=8)
  f.load_tasks(["img"] * 20)
  f.run(lambda t, h: None, strategy="naive", concurrency=8)
  assert f.handles() == []
  assert c.active_claims == 0


def test_per_task_error_is_captured_not_raised(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  reg = ClusterRegistry([c])
  f = _fleet(reg, max_concurrent=2)
  f.load_tasks(["a", "b", "c"])

  def pf(t, h):
    if t.image == "b":
      raise RuntimeError("boom")
    return t.image

  res = f.run(pf, strategy="naive", concurrency=2)
  assert res[0] == "a" and res[2] == "c"
  assert isinstance(res[1], RuntimeError)
  assert f.handles() == []   # all released despite the failure


def test_sliding_preserves_task_order(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["imgB", "imgA", "imgB"])   # processed grouped by image, window=1
  res = f.run(lambda t, h: t.image, strategy="sliding")
  assert res == ["imgB", "imgA", "imgB"]   # returned in original task order


# --- pipelined (double-buffered) ------------------------------------------ #
def test_pipelined_preserves_task_order(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["imgB", "imgA", "imgB"])
  res = f.run(lambda t, h: t.image, strategy="pipelined")
  assert res == ["imgB", "imgA", "imgB"]
  assert f.handles() == []


def test_pipelined_peak_at_most_two_windows(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["i1", "i2", "i3", "i4"])
  # a slow-ish process lets the prefetch of the next window overlap, so peak hits
  # 2 — but never 3 (single-slot prefetch + unwarm-before-await invariant).
  res = f.run(lambda t, h: time.sleep(0.05) or t.image, strategy="pipelined")
  assert res == ["i1", "i2", "i3", "i4"]
  assert 1 <= f.report.peak_warm <= 2   # window=1, reps=1 -> at most 2 windows
  assert f.handles() == []
  assert c.active_replicas == 0


def test_pipelined_overlaps_pull_with_process(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")

  def _slow_ready(*a, **k):
    time.sleep(0.1)
    return True
  c.resources.wait_for_pool_ready.side_effect = _slow_ready

  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["a", "b", "c", "d"])       # 4 windows of 1
  start = time.monotonic()
  f.run(lambda t, h: time.sleep(0.1), strategy="pipelined")
  elapsed = time.monotonic() - start
  # serial sliding would be ~4*(0.1 warm + 0.1 process) = 0.8s; pipelined overlaps
  # each window's process with the next window's pull -> ~0.1 + 4*0.1 = 0.5s.
  assert elapsed < 0.7


def test_pipelined_teardown_on_prefetch_failure(make_cluster):
  from agent_sandbox_rl import ClusterRegistry, FleetError
  c = make_cluster("solo")
  # window 0 becomes ready; the prefetch of window 1 never does -> FleetError.
  c.resources.wait_for_pool_ready.side_effect = [True, False, False, False]
  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["i1", "i2", "i3"])
  with pytest.raises(FleetError):
    f.run(lambda t, h: t.id, strategy="pipelined")
  # teardown still ran despite the background prefetch failure
  assert f.handles() == []
  assert c.active_claims == 0 and c.active_replicas == 0
