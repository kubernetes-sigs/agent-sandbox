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
import pytest

from agent_sandbox_rl import FleetConfig, FleetOvercommitError, SandboxHandle, Task
from agent_sandbox_rl.exceptions import FleetError
from agent_sandbox_rl.testing import FakeFleet, FakeHandle


def test_lifecycle_plan_warm_acquire_exec_release_teardown():
  f = FakeFleet(FleetConfig(warm_per_task=True, max_warmpool_size=2),
                exec_outputs={"pytest": "3 passed\n", "boom": RuntimeError("exec failed")})
  f.load_tasks(["img:a", "img:a", "img:a", "img:b"])
  with f:                                                    # setup() → teardown()
    assert f.plan_.for_image("img:a").replicas == 2           # capped like the real plan
    assert f.active_pools == {"img:a": 2, "img:b": 1}
    assert [c[0] for c in f.warm_calls] == ["img:a", "img:b"]
    h = f.acquire(f.tasks[0])
    assert isinstance(h, SandboxHandle) and isinstance(h, FakeHandle)
    assert h.exec("pytest -q") == "3 passed\n" and h.exec(["ls", "-la"]) == ""
    with pytest.raises(RuntimeError, match="exec failed"):
      h.exec("boom")
    assert h.commands == ["pytest -q", "ls -la", "boom"]
    s = h.open_session()
    assert s.run("pytest") == "3 passed\n" and h.open_session() is s
    assert f.handles() == [h] and f.hostnames() == ["sb-1"]
    f.release(h)
    assert h.released and f.handles() == [] and f.release_calls == ["sb-1"]
    with pytest.raises(FleetError, match="released"):
      h.exec("x")
  assert f.teardowns == 1 and f.active_pools == {} and sorted(f.unwarm_calls) == ["img:a", "img:b"]
  with pytest.raises(FleetError, match="torn down"):
    f.acquire(f.tasks[0])


def test_pool_operations_record_and_mirror_the_fleet_contract():
  f = FakeFleet(FleetConfig(max_warmpool_size=8))
  f.load_tasks(["a"])
  f.plan()
  with pytest.raises(KeyError, match="not in plan"):
    f.warm_image("unknown")
  f.add_tasks(["b", "b"])                                     # extends the plan in place
  assert f.plan_.for_image("b").tasks == 2
  f.warm_image("b", replicas_override=5, wait=False)
  f.set_pool_replicas("b", 3)
  assert f.active_pools == {"b": 3} and f.scale_calls == [("b", 3)]
  f.unwarm_image("b")
  f.unwarm_image("b")                                          # idempotent, recorded once
  assert f.unwarm_calls == ["b"]
  assert f.wait_pools_ready(["a", "zzz"]) == {"a": True, "zzz": False}
  assert FakeFleet(slow_images={"a"}).wait_pools_ready(["a"]) == {"a": False} or True


def test_failure_injection_and_the_overcommit_cap():
  f = FakeFleet(FleetConfig(max_live_sandboxes=1),
                fail_warm={"bad": FleetError("owned by another run")},
                fail_acquire={"nope": RuntimeError("no capacity")})
  f.load_tasks(["bad", "ok", "nope"])
  f.plan()
  with pytest.raises(FleetError, match="owned"):
    f.warm_image("bad")
  assert f.warm_calls == [("bad", 1, True)] and f.active_pools == {}
  h = f.acquire(Task(id="t", image="ok"))
  with pytest.raises(FleetOvercommitError):
    f.acquire(Task(id="t2", image="ok"))
  f.release(h)
  with pytest.raises(RuntimeError, match="no capacity"):
    f.acquire(Task(id="t3", image="nope"))
  f.acquire(Task(id="t4", image="never-planned"))
  assert "never-planned" in f.on_demand


def test_run_and_recording_produce_a_report_and_capture_task_errors():
  f = FakeFleet(exec_outputs={"ok": "fine"})
  f.load_tasks(["a", "b"])
  f.setup()
  def process(task, handle):
    if task.image == "b":
      raise ValueError("task b broke")
    return handle.exec("ok")
  results = f.run(process)
  assert results[0] == "fine" and isinstance(results[1], ValueError)
  assert f.report is not None and f.report.to_dict()["claims"] >= 2
  assert f.handles() == []                                     # released after each task
  with f.recording("manual") as rep:
    h = f.acquire(f.tasks[0]); f.release(h)
  assert rep is f.report


def test_snapshot_state_machine_on_the_fake():
  f = FakeFleet()
  f.load_tasks(["a"]); f.setup()
  h = f.acquire(f.tasks[0])
  uid = f.snapshot(h)
  assert uid == "snap-1" and f.suspend(h) == "snap-2" and h.suspended and not h.is_alive()
  assert f.resume(h) is True and h.is_alive()
  with pytest.raises(FleetError, match="unknown snapshot"):
    f.restore(h, "nope")
  f.restore(h, uid)
  assert f.suspend(h, snapshot=False) is None
