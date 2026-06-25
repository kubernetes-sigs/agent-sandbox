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
