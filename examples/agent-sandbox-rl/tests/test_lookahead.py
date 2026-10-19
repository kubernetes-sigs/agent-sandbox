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
"""LookaheadWarmer against a recording fake fleet, plus the fleet's add_tasks
and wait_pools_ready against the shared FakeCluster."""
import threading
import time
import types

import pytest

from agent_sandbox_rl import ClusterRegistry, FleetConfig, LookaheadWarmer, SandboxFleet, WarmReport
from agent_sandbox_rl.exceptions import FleetError, NoClusterAvailableError
from agent_sandbox_rl.preflight import PreflightReport
from agent_sandbox_rl.sources import Task


class RecordingFleet:
  """Records warm / scale / unwarm calls; images in `planned` are in the plan."""

  def __init__(self, planned=(), cap=16, fail=None):
    self.config = types.SimpleNamespace(max_warmpool_size=cap, ready_timeout=900)
    self.calls = []
    self.planned = set(planned)
    self.fail = fail or {}            # image -> exception to raise on warm
    self.wait_replicas = []           # the per-image share each barrier asked for
    self.plan_ = types.SimpleNamespace(
        for_image=lambda img: object() if img in self.planned else None)
    self._lock = threading.Lock()

  def add_tasks(self, tasks):
    with self._lock:
      self.calls.append(("add_tasks", tuple(t.image for t in tasks)))
      self.planned.update(t.image for t in tasks)
    return tasks

  def warm_image(self, image, *, replicas_override=None, wait=True):
    with self._lock:
      self.calls.append(("warm", image, replicas_override, wait))
    if image in self.fail:
      raise self.fail[image]

  def set_pool_replicas(self, image, replicas):
    with self._lock:
      self.calls.append(("scale", image, replicas))

  def unwarm_image(self, image):
    with self._lock:
      self.calls.append(("unwarm", image))

  def wait_pools_ready(self, images, *, timeout=None, replicas=None):
    with self._lock:
      self.calls.append(("wait", tuple(images), timeout))
      self.wait_replicas.append(replicas)
    return {img: img != "slow" for img in images}

  def of(self, kind):
    return sorted(c for c in self.calls if c[0] == kind)


def test_first_batch_warms_each_image_capped_without_waiting():
  f = RecordingFleet(planned={"a", "b"})
  w = LookaheadWarmer(f, cap=8)
  rep = w.add(0, {"a": 4, "b": 32})
  assert rep.ok and sorted(rep.created) == ["a", "b"]
  assert f.of("warm") == [("warm", "a", 4, False), ("warm", "b", 8, False)]
  assert w.active == {"a": 4, "b": 8} and w.desired() == {"a": 4, "b": 8}
  assert "created=2" in rep.summary()


def test_overlapping_batches_scale_and_finished_batches_keep_their_pools_for_the_window():
  f = RecordingFleet(planned={"a", "b", "c"})
  w = LookaheadWarmer(f, cap=16, keep_finished=1)
  w.add(0, {"a": 4, "b": 4})
  rep = w.add(1, {"a": 4, "c": 2})          # a is in both: 8; c is new
  assert rep.scaled == {"a": 8} and rep.created == ["c"] and not rep.removed
  rep = w.finish(0)                          # kept: batch 0 still counts
  assert rep.scaled == {} and rep.removed == [] and w.active == {"a": 8, "b": 4, "c": 2}
  w.add(2, {"a": 4})
  rep = w.finish(1)                          # ring holds batch 1 only; batch 0's b goes
  assert rep.removed == ["b"] and w.active["a"] == 8      # a: kept batch 1 (4) + batch 2 (4)
  assert ("unwarm", "b") in f.calls
  rep = w.finish(2)                          # ring holds batch 2; c (batch 1) goes
  assert rep.removed == ["c"] and w.active == {"a": 4}
  assert ("scale", "a", 4) in f.calls


def test_finish_without_a_window_unwarms_at_once_and_unknown_ids_are_noops():
  f = RecordingFleet(planned={"a"})
  w = LookaheadWarmer(f)
  w.add(0, {"a": 2})
  assert w.finish("nope") == WarmReport()
  rep = w.finish(0)
  assert rep.removed == ["a"] and w.active == {} and w.live_batches == []


def test_unknown_images_are_registered_first_unless_disabled():
  f = RecordingFleet(planned={"a"})
  LookaheadWarmer(f).add(0, {"a": 1, "new": 1})
  assert ("add_tasks", ("new",)) in f.calls
  f2 = RecordingFleet(planned={"a"})
  LookaheadWarmer(f2, register_images=False).add(0, {"new": 1})
  assert not f2.of("add_tasks") and ("warm", "new", 1, False) in f2.calls


def test_warm_errors_are_recorded_not_raised_unless_fail_fast():
  f = RecordingFleet(planned={"a", "b"}, fail={"a": RuntimeError("boom")})
  rep = LookaheadWarmer(f).add(0, {"a": 1, "b": 1})
  assert not rep.ok and "boom" in rep.errors["a"] and rep.created == ["b"]
  f2 = RecordingFleet(planned={"a", "b"}, fail={"a": FleetError("owned by run x")})
  w = LookaheadWarmer(f2, fail_fast=True)
  with pytest.raises(FleetError, match="owned by run x"):
    w.add(0, {"a": 1, "b": 1})
  assert ("warm", "b", 1, False) in f2.calls      # the phase still ran for the rest
  assert w.active == {"b": 1}
  # a plain error is never fatal, even under fail_fast
  f3 = RecordingFleet(planned={"a"}, fail={"a": RuntimeError("blip")})
  assert "blip" in LookaheadWarmer(f3, fail_fast=True).add(0, {"a": 1}).errors["a"]


def test_wait_runs_the_best_effort_barrier_for_the_batch_only():
  f = RecordingFleet(planned={"a", "slow", "b"})
  w = LookaheadWarmer(f)
  w.add(0, {"b": 1})
  rep = w.add(1, {"a": 1, "slow": 1}, wait=True, wait_timeout=30)
  assert rep.ready == {"a": True, "slow": False}
  assert f.of("wait") == [("wait", ("a", "slow"), 30)]
  assert w.wait_ready() == {"a": True, "slow": False, "b": True}   # default: all active


def test_add_async_returns_a_future_and_close_unwarms_everything():
  f = RecordingFleet(planned={"a", "b"})
  with LookaheadWarmer(f) as w:
    fut = w.add_async(0, {"a": 2})
    assert isinstance(fut.result(timeout=5), WarmReport) and w.active == {"a": 2}
    w.add(1, {"b": 1})
  assert sorted(c[1] for c in f.calls if c[0] == "unwarm") == ["a", "b"]
  assert w.active == {}


def test_re_adding_a_batch_replaces_its_counts_and_zero_counts_are_dropped():
  f = RecordingFleet(planned={"a", "b"})
  w = LookaheadWarmer(f)
  w.add(0, {"a": 4, "b": 0})
  assert w.active == {"a": 4}
  rep = w.add(0, {"a": 2})
  assert rep.scaled == {"a": 2} and w.active == {"a": 2}


# ---------------------------------------------------------------- fleet side


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _fleet(c, **cfg):
  return SandboxFleet(FleetConfig(warm_per_task=True, max_warmpool_size=4, **cfg),
                      registry=ClusterRegistry([c]))


def test_add_tasks_extends_the_plan_in_place(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks(["a", "a", "b"])
  plan = f.plan()
  added = f.add_tasks(["c", "c", "c", "a"])
  assert [t.image for t in added] == ["c", "c", "c", "a"]
  assert f.plan_ is plan                                   # same object, extended
  assert plan.for_image("c").replicas == 3 and plan.for_image("c").tasks == 3
  assert plan.for_image("a").tasks == 3                    # count grows, size untouched
  assert len(plan.entries) == 3 and len(f.tasks) == 7
  assert plan.for_image("c").pool == f.config.pool_name("c")
  # capped like plan(): warm_per_task but max_warmpool_size=4
  f.add_tasks(["d"] * 9)
  assert plan.for_image("d").replicas == 4
  f.warm_image("c", wait=False)                            # the entry is usable
  assert ("pool-r2e-img-" in c.resources.create_warmpool.call_args.args[0])


def test_add_tasks_before_plan_only_appends_and_accepts_rewrites(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks(["a"])
  added = f.add_tasks(["docker.io/library/b:1"], image_rewrite=lambda i: i.replace("docker.io/library/", "m/"))
  assert added[0].image == "m/b:1" and added[0].metadata["original_image"] == "docker.io/library/b:1"
  assert f.plan_ is None and [t.image for t in f.tasks] == ["a", "m/b:1"]
  assert f.add_tasks([]) == []


def test_add_tasks_in_adopt_mode_replans_against_the_namespace(make_cluster, monkeypatch):
  c = make_cluster("solo")
  f = _fleet(c, adopt_existing=True)
  f.load_tasks(["a"])
  calls = []
  monkeypatch.setattr(f, "_plan_adopt", lambda: calls.append("replan"))
  f.plan_ = types.SimpleNamespace(entries=[], for_image=lambda img: None)
  f.add_tasks(["b"])
  assert calls == ["replan"]


def test_wait_pools_ready_is_best_effort_and_uses_what_the_fleet_asked_for(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks(["a", "a", "b", "c"])
  f.plan()
  f.warm_image("a", wait=False)                            # a: 2 replicas warmed
  f.warm_image("b", replicas_override=3, wait=False)
  seen = {}
  def fake_wait(pool, replicas, timeout=None):
    seen[pool] = (replicas, timeout)
    if pool == f.config.pool_name("b"):
      raise RuntimeError("watch dropped")
    return True
  c.resources.wait_for_pool_ready.side_effect = fake_wait
  ready = f.wait_pools_ready(["a", "b", "c", "zzz"], timeout=12)
  assert ready == {"a": True, "b": False, "c": True, "zzz": False}
  assert seen[f.config.pool_name("a")] == (2, 12)
  assert seen[f.config.pool_name("b")] == (3, 12)
  assert seen[f.config.pool_name("c")] == (1, 12)          # not warmed by us: one ready replica
  assert f.wait_pools_ready([]) == {}


class SlowWarmFleet(RecordingFleet):
  """warm_image takes a while, so a second add_async is still queued."""

  def warm_image(self, image, *, replicas_override=None, wait=True):
    time.sleep(0.2)
    super().warm_image(image, replicas_override=replicas_override, wait=wait)


def test_close_drains_or_cancels_a_queued_add_before_it_clears_the_tables():
  f = SlowWarmFleet(planned={"a", "b"})
  w = LookaheadWarmer(f)
  w.add_async(0, {"a": 2})
  w.add_async(1, {"b": 1})                    # queued behind the first
  w.close()
  assert w.active == {} and w.live_batches == []
  warmed = {c[1] for c in f.calls if c[0] == "warm"}
  unwarmed = {c[1] for c in f.calls if c[0] == "unwarm"}
  assert warmed and unwarmed == warmed        # nothing warmed survives close()


def test_wait_uses_this_batchs_share_not_the_sum_across_batches():
  f = RecordingFleet(planned={"a"})
  w = LookaheadWarmer(f, cap=4)
  w.add(0, {"a": 2})
  w.add(1, {"a": 3}, wait=True)
  assert w.active == {"a": 4}                  # capped sum
  assert f.wait_replicas == [{"a": 3}]         # this batch alone, below the cap
  w.add(2, {"a": 9}, wait=True)
  assert f.wait_replicas[-1] == {"a": 4}       # a batch above the cap waits for the cap


def test_add_async_registers_the_batch_before_the_worker_runs():
  f = RecordingFleet(planned={"a"})
  w = LookaheadWarmer(f)
  w.add(0, {"a": 1})
  fut = w.add_async(1, {"a": 1})               # shares the image with batch 0
  assert 1 in w.live_batches                   # known at once, not when the worker runs
  w.finish(0)                                  # must not unwarm "a": batch 1 still wants it
  fut.result(timeout=5)
  assert w.active == {"a": 1}
  assert not [c for c in f.calls if c[0] == "unwarm"]
  w.close()


def test_lookahead_unwraps_an_async_fleet():
  inner = RecordingFleet(planned={"a"})
  assert LookaheadWarmer(types.SimpleNamespace(_fleet=inner)).fleet is inner
  assert LookaheadWarmer(inner).fleet is inner


def test_add_tasks_leaves_tasks_and_plan_untouched_when_placement_fails(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks(["a"])
  plan = f.plan()

  class NoRoom:
    def select(self, image, registry):
      raise NoClusterAvailableError("every cluster is at max_replicas")

  f.placement = NoRoom()
  with pytest.raises(NoClusterAvailableError):
    f.add_tasks(["a", "zzz"])                 # known image first, then the one that fails
  assert len(f.tasks) == 1 and plan.for_image("a").tasks == 1
  assert plan.for_image("zzz") is None and f.plan_ is plan


def test_wait_pools_ready_without_a_plan_reads_false(make_cluster, monkeypatch):
  c = make_cluster("solo")
  f = _fleet(c)

  def no_plan():
    raise FleetError("run namespace cannot be created")

  monkeypatch.setattr(f, "plan", no_plan)
  assert f.wait_pools_ready(["a", "b"]) == {"a": False, "b": False}
