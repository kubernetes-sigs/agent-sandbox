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

"""The FakeFleet test double behaves like the fleet where a framework's loop can tell."""
import inspect

import pytest

from agent_sandbox_rl import FleetConfig, FleetOvercommitError, SandboxFleet, SandboxHandle, Task, sizing
from agent_sandbox_rl.config import run_namespace
from agent_sandbox_rl.exceptions import FleetError
from agent_sandbox_rl.testing import FakeFleet, FakeHandle, FakeSandboxGone


def test_lifecycle_plan_warm_acquire_exec_release_teardown():
  f = FakeFleet(FleetConfig(warm_per_task=True, max_warmpool_size=2),
                exec_outputs={"pytest": "3 passed\n", "pytest -x": "1 passed\n",
                              "boom": RuntimeError("exec failed")})
  f.load_tasks(["img:a", "img:a", "img:a", "img:b"])
  with f:                                                    # setup() → teardown()
    assert f.plan_.for_image("img:a").replicas == 2           # capped like the real plan
    assert f.active_pools == {"img:a": 2, "img:b": 1}
    assert [c[0] for c in f.warm_calls] == ["img:a", "img:b"]
    h = f.acquire(f.tasks[0])
    assert isinstance(h, SandboxHandle) and isinstance(h, FakeHandle)
    assert h.exec("pytest -q") == "3 passed\n" and h.exec(["ls", "-la"]) == ""
    assert h.exec("pytest -x") == "1 passed\n"                 # longest key wins
    with pytest.raises(RuntimeError, match="exec failed"):
      h.exec("boom")
    assert h.commands == ["pytest -q", "ls -la", "pytest -x", "boom"]
    s = h.open_session()
    assert s.run("pytest") == "3 passed\n" and h.open_session() is s
    assert h.endpoint(8080) == "sb-1.default:8080"              # inherited, works
    assert f.handles() == [h] and f.hostnames() == ["sb-1"] and f.endpoints() == ["sb-1.default:8888"]
    f.release(h)
    assert h.released and f.handles() == [] and f.release_calls == ["sb-1"]
    with pytest.raises(FakeSandboxGone):                        # fake-only guard
      h.exec("x")
  assert f.teardowns == 1 and f.active_pools == {} and sorted(f.unwarm_calls) == ["img:a", "img:b"]
  assert f.plan_ is None
  with pytest.raises(FleetError, match="torn down"):
    f.acquire(f.tasks[0])


def test_the_fake_offers_only_methods_the_real_fleet_has_with_the_same_signatures():
  for name, member in inspect.getmembers(FakeFleet, callable):
    if name.startswith("_"):
      continue
    assert hasattr(SandboxFleet, name), f"FakeFleet.{name} has no SandboxFleet counterpart"
    real = inspect.signature(getattr(SandboxFleet, name))
    fake = inspect.signature(member)
    real_names = [p for p in real.parameters if p not in ("self",)]
    fake_names = [p for p in fake.parameters if p not in ("self", "recycle_options")]
    assert fake_names == real_names[:len(fake_names)], (name, fake_names, real_names)
  def shape(fn):
    return [(q.name, q.kind, q.default) for q in inspect.signature(fn).parameters.values()]

  for name in ("exec", "open_session", "close_session", "refresh", "snapshot", "suspend",
               "resume", "restore", "list_snapshots", "snapshot_deleter", "release", "endpoint"):
    assert shape(getattr(FakeHandle, name)) == shape(getattr(SandboxHandle, name)), name
  assert isinstance(inspect.getattr_static(FakeHandle, "is_suspended"), property)


def test_pool_operations_mirror_the_fleet_contract():
  f = FakeFleet(FleetConfig(max_warmpool_size=8))
  f.load_tasks(["a", "b"])
  f.plan()
  with pytest.raises(KeyError, match="not in plan"):
    f.warm_image("unknown")
  f.warm_images(["a", "zzz"])                                  # skips, never raises
  assert f.active_pools == {"a": 1} and f.skipped_images == ["zzz"]
  f.set_pool_replicas("b", 3)                                  # planned, not yet warmed: upsert
  f.set_pool_replicas("zzz", 3)                                # unplanned: silent no-op
  assert f.active_pools == {"a": 1, "b": 3} and f.scale_calls == [("b", 3)]
  f.unwarm_image("b")
  f.unwarm_image("b")                                          # idempotent, recorded once
  assert f.unwarm_calls == ["b"]


def test_plan_sizes_pools_like_the_real_fleet():
  cfg = FleetConfig(max_concurrent=4, max_warmpool_size=8)
  f = FakeFleet(cfg)
  f.load_tasks(["a", "a", "b", "c"])
  plan = f.plan()
  want = {img: sizing.compute_replicas(n, 4, 4, 8) for img, n in {"a": 2, "b": 1, "c": 1}.items()}
  assert {e.image: e.replicas for e in plan.entries} == want
  assert f.recommended_window() >= 1                           # the real window arithmetic
  assert plan.for_image("a").pool == f.config.pool_name("a")


def test_config_is_resolved_like_the_real_fleet():
  f = FakeFleet(FleetConfig(pool_name_format="{template}-{run_id}", run_id="abc123"))
  f.load_tasks(["a"])
  assert f.plan().entries[0].pool.endswith("-abc123") and f.run_id == "abc123"
  g = FakeFleet(FleetConfig(run_isolation="namespace", run_id="abc123"))
  assert g.namespace == run_namespace("default", "abc123")
  g.load_tasks(["a"]); g.setup()
  assert g.endpoints(80) == [] and g.acquire(g.tasks[0]).endpoint(80).endswith(f".{g.namespace}:80")
  assert FakeFleet(FleetConfig()).config.labels["agents.x-k8s.io/asrl-run-id"] == "fake-run"


def test_failure_injection_and_the_overcommit_cap():
  f = FakeFleet(FleetConfig(max_live_sandboxes=1),
                fail_warm={"bad": FleetError("owned by another run")},
                fail_acquire={"nope": RuntimeError("no capacity")})
  f.load_tasks(["bad", "ok", "nope"])
  f.plan()
  with pytest.raises(FleetError, match="owned"):
    f.warm_image("bad")
  assert f.warm_calls == [("bad", 1, True)] and f.active_pools == {}
  h = f.acquire(Task(id="t", image="ok"), snapshot_uid="snap-1")
  assert f.acquire_calls[-1] == ("ok", "snap-1", None)
  with pytest.raises(FleetOvercommitError):
    f.acquire(Task(id="t2", image="ok"))
  with pytest.raises(TypeError):                               # a typo fails here too
    f.acquire(Task(id="t3", image="ok"), snapshot_id="x")
  f.release(h)
  with pytest.raises(RuntimeError, match="no capacity"):
    f.acquire(Task(id="t3", image="nope"))
  f.acquire(Task(id="t4", image="never-planned"))
  assert "never-planned" in f.on_demand


def test_run_executes_the_real_strategies_and_reports_like_the_fleet():
  f = FakeFleet(FleetConfig(max_concurrent=2), exec_outputs={"ok": "fine"})
  f.load_tasks(["a", "b", "a"])

  def process(task, handle):
    if task.image == "b":
      raise ValueError("task b broke")
    return handle.exec("ok")

  results = f.run(process)
  assert results[0] == "fine" and isinstance(results[1], ValueError) and results[2] == "fine"
  assert (f.report.tasks_ok, f.report.tasks_err) == (2, 1)
  assert "process" in f.report.phases and f.report.environment["fake"]["fake"] is True
  assert f.handles() == [] and f.teardowns == 1 and f.plan_ is None   # torn down, as the real run
  with pytest.raises(FleetError, match="torn down"):
    f.acquire(f.tasks[0])
  with pytest.raises(ValueError, match="unknown strategy"):
    f.run(process, strategy="bogus")
  with pytest.raises(NotImplementedError):
    f.run(process, recycle=True)
  # epochs: one list per pass, pools kept between passes, one teardown at the end
  g = FakeFleet(FleetConfig())
  g.load_tasks(["a", "b"])
  out = g.run(lambda t, h: t.image, epochs=2)
  assert out == [["a", "b"], ["a", "b"]] and g.teardowns == 1
  # keep_warm: no teardown, the caller's loop reuses the pools
  k = FakeFleet(FleetConfig())
  k.load_tasks(["a"])
  k.run(lambda t, h: None, keep_warm=True)
  assert k.teardowns == 0 and k.active_pools == {"a": 1}
  k.teardown()
  # the windowed strategies run end to end over the fake
  for strategy in ("sliding", "pipelined"):
    w = FakeFleet(FleetConfig(max_concurrent=2, window_size=1))
    w.load_tasks(["a", "b", "c"])
    assert w.run(lambda t, h: t.image, strategy=strategy) == ["a", "b", "c"]
    assert w.teardowns == 1 and w.unwarm_calls


def test_suspend_frees_the_claim_slot_and_resume_retakes_it():
  f = FakeFleet(FleetConfig(max_live_sandboxes=1))
  f.load_tasks(["a", "b"]); f.setup()
  h = f.acquire(f.tasks[0])
  uid = f.suspend(h)
  assert uid and h.is_suspended and h.list_snapshots() == [uid]
  with pytest.raises(FakeSandboxGone, match="suspended"):
    h.exec("x")
  other = f.acquire(f.tasks[1])                               # the slot is free now
  with pytest.raises(FleetOvercommitError):
    f.resume(h)                                               # at the cap: stays suspended
  assert h.is_suspended
  f.release(other)
  pod_before = h.pod_name
  assert f.resume(h) is True and not h.is_suspended and h.pod_name != pod_before
  f.release(h)
  assert f._claims_reserved == 0
  # restore: unknown uid raises, known one wakes the handle; suspend(snapshot=False)
  g = FakeFleet(); g.load_tasks(["a"]); g.setup()
  h = g.acquire(g.tasks[0])
  uid = g.snapshot(h, "pre")
  assert uid.startswith("pre-")
  assert g.suspend(h, snapshot=False) is None and h.is_suspended
  with pytest.raises(FleetError, match="unknown snapshot"):
    g.restore(h, "nope")
  g.restore(h, uid)
  assert not h.is_suspended
  h.snapshot_deleter()(uid)
  assert h.list_snapshots() == []
  g.release(h, delete_snapshots=True)


def test_recording_wraps_manual_acquires_in_a_report():
  f = FakeFleet(); f.load_tasks(["a"]); f.setup()
  with f.recording("manual") as rep:
    h = f.acquire(f.tasks[0]); f.release(h)
  assert rep is f.report and rep.to_dict()["claims"] == 1


def test_snapshot_calls_on_a_released_handle_fail_and_leave_the_slot_count_alone():
  f = FakeFleet(FleetConfig(max_live_sandboxes=2))
  f.load_tasks(["a", "b"]); f.setup()
  h = f.acquire(f.tasks[0])
  other = f.acquire(f.tasks[1])
  f.release(h)
  for call in (lambda: f.suspend(h), lambda: f.resume(h), lambda: f.restore(h, "x"),
               lambda: f.snapshot(h)):
    with pytest.raises(FakeSandboxGone):
      call()
  assert f._claims_reserved == 1                           # only `other` holds a slot
  f.acquire(Task(id="t3", image="a"))                      # exactly one slot was free
  with pytest.raises(FleetOvercommitError):
    f.acquire(Task(id="t4", image="a"))
  # suspended, then released: the slot was given back once, and a late wake does not retake it
  g = FakeFleet(FleetConfig(max_live_sandboxes=1))
  g.load_tasks(["a"]); g.setup()
  h = g.acquire(g.tasks[0])
  g.suspend(h)
  g.release(h)
  with pytest.raises(FakeSandboxGone):
    g.resume(h)
  assert g._claims_reserved == 0 and g.acquire(g.tasks[0])
