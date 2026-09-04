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

"""Tests for the GKE Pod Snapshot primitives (handle methods, fleet wrappers,
acquire pin, client construction). Everything runs against fakes that mimic the
SDK extension's result objects — no cluster, no kubeconfig."""

import sys
import types
from unittest.mock import MagicMock

import pytest

from agent_sandbox_rl import (
    ClusterRegistry,
    FleetConfig,
    SandboxFleet,
    SnapshotError,
    SnapshotsUnavailable,
    constants,
)
from agent_sandbox_rl import snapshots as snap
from agent_sandbox_rl.handles import SandboxHandle
from agent_sandbox_rl.preflight import PreflightReport
from agent_sandbox_rl.sources import Task

PIN = constants.PODSNAPSHOT_NAME_ANNOTATION


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _ok(**kw):
  return types.SimpleNamespace(success=True, error_reason="", **kw)


def _fail(reason):
  return types.SimpleNamespace(success=False, error_reason=reason)


class FakeEngine:
  """Mimics the extension's SnapshotEngine result contract."""

  def __init__(self):
    self.created, self.deleted, self.deleted_all = [], [], 0

  def create(self, trigger_name, podsnapshot_timeout=180):
    self.created.append((trigger_name, podsnapshot_timeout))
    return _ok(snapshot_uid=f"uid-{len(self.created)}", trigger_name=trigger_name)

  def list(self, filter_by=None):
    return _ok(snapshots=[types.SimpleNamespace(snapshot_uid="uid-1", status="Ready")])

  def delete(self, snapshot_uid, timeout=180):
    self.deleted.append((snapshot_uid, timeout))
    return _ok(deleted_snapshots=[snapshot_uid])

  def delete_all(self, delete_by="all", timestamp=None, timeout=180):
    self.deleted_all += 1
    return _ok(deleted_snapshots=["uid-1"])


class FakeSnapSandbox:
  """Mimics SandboxWithSnapshotSupport: result objects, new pod on resume."""

  def __init__(self):
    self.claim_name, self.sandbox_id = "claim-1", "sb-1"
    self.snapshots = FakeEngine()
    self._suspended, self._pod = False, 1
    self.calls, self.fail_next = [], None

  def get_pod_name(self):
    return f"pod-{self._pod}"

  def get_pod_ip(self):
    return f"10.0.0.{self._pod}"

  def is_suspended(self):
    return self._suspended

  def _maybe_fail(self):
    if self.fail_next:
      reason, self.fail_next = self.fail_next, None
      return _fail(reason)
    return None

  def suspend(self, snapshot_before_suspend=True, wait_timeout=180):
    self.calls.append(("suspend", snapshot_before_suspend, wait_timeout))
    if (f := self._maybe_fail()) is not None:
      return f
    self._suspended = True
    sr = _ok(snapshot_uid="uid-s") if snapshot_before_suspend else None
    return _ok(snapshot_response=sr)

  def resume(self, wait_timeout=180):
    self.calls.append(("resume", wait_timeout))
    if (f := self._maybe_fail()) is not None:
      return f
    self._suspended, self._pod = False, self._pod + 1
    return _ok(restored_from_snapshot=True, snapshot_uid="uid-s")

  def restore(self, snapshot_uid, sandbox_ready_timeout=180):
    self.calls.append(("restore", snapshot_uid, sandbox_ready_timeout))
    if (f := self._maybe_fail()) is not None:
      return f
    self._suspended, self._pod = False, self._pod + 1
    return _ok(restored_from_snapshot=True, snapshot_uid=snapshot_uid)

  def terminate(self):
    self.calls.append(("terminate",))


class FakeSession:
  def __init__(self):
    self.is_open, self.closed = True, 0

  def close(self):
    self.is_open, self.closed = False, self.closed + 1


def _handle(sandbox=None, cluster=None):
  return SandboxHandle(
      task=Task(id="t", image="img", metadata={}), cluster_name="c",
      claim_name="claim-1", sandbox_id="sb-1", pod_name="pod-1", hostname="sb-1",
      pod_ip="10.0.0.1", sandbox=sandbox if sandbox is not None else FakeSnapSandbox(),
      _cluster=cluster or types.SimpleNamespace(namespace="ns"))


# ------------------------------------------------------------------ handle


def test_snapshot_returns_uid_and_names_trigger_after_sandbox():
  h = _handle()
  assert h.snapshot() == "uid-1"
  assert h.sandbox.snapshots.created == [("snap-sb-1", 180)]
  assert h.snapshot("pristine", timeout=30) == "uid-2"
  assert h.sandbox.snapshots.created[-1] == ("pristine", 30)


def test_suspend_forwards_flags_closes_session_and_returns_uid():
  h = _handle()
  s = FakeSession()
  h._session = s
  assert h.suspend(timeout=60) == "uid-s"
  assert h.sandbox.calls == [("suspend", True, 60)]
  assert s.closed == 1 and h._session is None   # stream bound to the deleted pod
  assert h.is_suspended


def test_suspend_without_snapshot_returns_none():
  h = _handle()
  assert h.suspend(snapshot=False) is None
  assert h.sandbox.calls == [("suspend", False, 180)]


def test_failure_raises_snapshot_error_with_reason():
  h = _handle()
  h.sandbox.fail_next = "PodSnapshotPolicy not found"
  with pytest.raises(SnapshotError, match="suspend failed: PodSnapshotPolicy not found"):
    h.suspend()
  assert not h.is_suspended                      # a failed suspend never reads as parked


def test_resume_refreshes_pod_identity_and_drops_session():
  h = _handle()
  h.suspend()
  h._session = FakeSession()                     # e.g. re-opened by a careless caller
  assert h.resume(timeout=90) is True
  assert h.sandbox.calls[-1] == ("resume", 90)
  assert h.pod_name == "pod-2" and h.pod_ip == "10.0.0.2"     # new pod
  assert h.hostname == "sb-1" and h.sandbox_id == "sb-1"      # stable identity
  assert h._session is None


def test_restore_pins_uid_and_refreshes():
  h = _handle()
  h.suspend()
  h.restore("uid-7", timeout=45)
  assert h.sandbox.calls[-1] == ("restore", "uid-7", 45)
  assert h.pod_name == "pod-2"


def test_list_and_delete_snapshots():
  h = _handle()
  assert [s.snapshot_uid for s in h.list_snapshots()] == ["uid-1"]
  h.delete_snapshot("uid-1", timeout=10)
  assert h.sandbox.snapshots.deleted == [("uid-1", 10)]
  h.delete_snapshots()
  assert h.sandbox.snapshots.deleted_all == 1


def test_plain_sandbox_raises_snapshots_unavailable():
  plain = types.SimpleNamespace(claim_name="c", sandbox_id="s",
                                get_pod_name=lambda: "p", get_pod_ip=lambda: None)
  h = _handle(sandbox=plain)
  for call in (h.snapshot, h.suspend, h.resume, lambda: h.restore("u"),
               h.list_snapshots, h.delete_snapshots):
    with pytest.raises(SnapshotsUnavailable, match="ClusterConfig\\(snapshots=True\\)"):
      call()
  with pytest.raises(SnapshotsUnavailable):
    h.is_suspended  # noqa: B018 — property access is the call under test


def test_refresh_keeps_last_pod_name_when_reread_fails():
  h = _handle()
  h.sandbox.get_pod_name = lambda: (_ for _ in ()).throw(RuntimeError("api down"))
  h.refresh()
  assert h.pod_name == "pod-1" and h.pod_ip == "10.0.0.1"


# ------------------------------------------------------------------- fleet


def _fleet(registry, **cfg):
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def test_acquire_pins_snapshot_uid_via_pod_annotations(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  f.acquire(f.tasks[0], snapshot_uid="uid-9", pod_annotations={"x": "y"})
  kw = c.sandbox_client.create_sandbox.call_args.kwargs
  assert kw["pod_annotations"] == {"x": "y", PIN: "uid-9"}
  assert kw["labels"]                                # existing kwargs untouched


def test_acquire_without_pin_passes_no_pod_annotations(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  f.acquire(f.tasks[0])
  assert "pod_annotations" not in c.sandbox_client.create_sandbox.call_args.kwargs


def test_fleet_wrappers_time_phases_and_count_restores(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  h = f.acquire(f.tasks[0])
  h.sandbox = FakeSnapSandbox()
  with f._obs.run("test") as rep:
    assert f.snapshot(h, "pre") == "uid-1"
    assert f.suspend(h, timeout=30) == "uid-s"
    assert f.resume(h) is True
    f.suspend(h, snapshot=False)
    f.restore(h, "uid-1")
  assert {"snapshot", "suspend", "resume", "restore"} <= set(rep.phases)
  assert rep.phases["suspend"][0] == 2
  assert rep.snap_restored == 2 and rep.snap_cold == 0
  assert rep.to_dict()["snapshots"] == {"restored": 2, "cold": 0}
  assert "snapshot resumes: restored=2 cold=0" in rep.summary()
  assert h.pod_name == "pod-3"                       # refreshed after each new pod


def test_release_deletes_snapshots_only_when_asked(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img", "img"])
  keep, drop = f.acquire(f.tasks[0]), f.acquire(f.tasks[1])
  keep.sandbox, drop.sandbox = FakeSnapSandbox(), FakeSnapSandbox()
  f.release(keep)
  assert keep.sandbox.snapshots.deleted_all == 0 and ("terminate",) in keep.sandbox.calls
  f.release(drop, delete_snapshots=True)
  assert drop.sandbox.snapshots.deleted_all == 1 and ("terminate",) in drop.sandbox.calls
  assert f.handles() == []


def test_release_snapshot_cleanup_failure_does_not_block_release(make_cluster, caplog):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  h = f.acquire(f.tasks[0])
  h.sandbox = FakeSnapSandbox()
  h.sandbox.snapshots.delete_all = lambda **kw: _fail("bucket gone")
  f.release(h, delete_snapshots=True)
  assert ("terminate",) in h.sandbox.calls
  assert f.handles() == []
  assert any("could not delete snapshots" in r.message for r in caplog.records)


# --------------------------------------------------------------- restore status


def _pod_with(conditions):
  return types.SimpleNamespace(status=types.SimpleNamespace(conditions=conditions))


def test_pod_restore_status_reads_condition():
  core = MagicMock()
  core.read_namespaced_pod.return_value = _pod_with([
      types.SimpleNamespace(type="Ready", status="True", message=""),
      types.SimpleNamespace(type="PodRestored", status="True",
                            message="restored from snapshot uid-42"),
  ])
  st = snap.pod_restore_status(core, "pod", "ns")
  assert st.restored is True
  assert st.from_snapshot("uid-42") and not st.from_snapshot("uid-1")
  assert not st.from_snapshot("uid-4")               # prefix of uid-42 must not match
  assert not st.from_snapshot("id-42")               # nor a suffix
  assert st.from_snapshot()                          # no uid → any restore counts
  st2 = snap.RestoreStatus(True, "PodSnapshot golden-v1-20260903-ab12 restored (uid=x.y)")
  assert st2.from_snapshot("golden-v1-20260903-ab12") and st2.from_snapshot("x.y")
  assert not st2.from_snapshot("golden-v1")


def test_pod_restore_status_fresh_and_unreadable():
  core = MagicMock()
  core.read_namespaced_pod.return_value = _pod_with([])
  assert snap.pod_restore_status(core, "pod", "ns").restored is False
  core.read_namespaced_pod.side_effect = RuntimeError("403")
  st = snap.pod_restore_status(core, "pod", "ns")
  assert st.restored is None and "403" in st.message and not st.from_snapshot()


# ------------------------------------------------------------------ client


class _FakeBaseClient:
  """Stands in for the SDK SandboxClient: records kwargs, sets an ambient helper."""

  def __init__(self, **kwargs):
    self.init_kwargs = kwargs
    self.k8s_helper = "ambient-helper"


class _FakeExtClient(_FakeBaseClient):
  """Stands in for PodSnapshotSandboxClient: checks the CRD via self.k8s_helper."""

  def __init__(self, *a, **kw):  # the stock ctor would check too early — must be bypassed
    raise AssertionError("stock PodSnapshotSandboxClient.__init__ must not run")

  def _check_snapshot_crd_installed(self):
    self.k8s_helper.checked += 1
    return self.k8s_helper.crd_ok


@pytest.fixture
def fake_sdk(monkeypatch):
  import k8s_agent_sandbox
  monkeypatch.setattr(k8s_agent_sandbox, "SandboxClient", _FakeBaseClient)
  mod = types.ModuleType("k8s_agent_sandbox.gke_extensions.snapshots")
  mod.PodSnapshotSandboxClient = _FakeExtClient
  monkeypatch.setitem(sys.modules, "k8s_agent_sandbox.gke_extensions.snapshots", mod)


def _helper(crd_ok):
  return types.SimpleNamespace(crd_ok=crd_ok, checked=0)


def test_build_snapshot_client_injects_helper_before_crd_check(fake_sdk):
  helper = _helper(True)
  c = snap.build_snapshot_client(helper, tracer_config="tc")
  assert c.k8s_helper is helper                     # not the ambient one
  assert helper.checked == 1                        # checked against THIS cluster
  assert c.init_kwargs == {"tracer_config": "tc"}
  assert snap.build_snapshot_client(_helper(True)).init_kwargs == {}


def test_build_snapshot_client_fails_clearly_without_crd(fake_sdk):
  with pytest.raises(SnapshotsUnavailable, match="PodSnapshot CRD"):
    snap.build_snapshot_client(_helper(False))


def test_build_snapshot_client_without_extension(monkeypatch):
  monkeypatch.setitem(sys.modules, "k8s_agent_sandbox.gke_extensions.snapshots", None)
  with pytest.raises(SnapshotsUnavailable, match="gke_extensions.snapshots"):
    snap.build_snapshot_client(_helper(True))


def test_cluster_builds_snapshot_client_when_configured(monkeypatch):
  from agent_sandbox_rl import Cluster, ClusterConfig
  helper = _helper(True)
  monkeypatch.setattr(Cluster, "k8s_helper", property(lambda self: helper))
  built = {}

  def fake_build(k8s_helper, *, tracer_config=None):
    built["helper"], built["tracer"] = k8s_helper, tracer_config
    return "snapshot-client"
  monkeypatch.setattr(snap, "build_snapshot_client", fake_build)
  c = Cluster(ClusterConfig(name="gke", snapshots=True), api_client=MagicMock())
  assert c.sandbox_client == "snapshot-client"
  assert built == {"helper": helper, "tracer": None}
  assert c.sandbox_client == "snapshot-client"      # cached


def test_cluster_default_client_path_unchanged(monkeypatch):
  from agent_sandbox_rl import Cluster, ClusterConfig
  import k8s_agent_sandbox
  monkeypatch.setattr(Cluster, "k8s_helper", property(lambda self: "helper"))
  monkeypatch.setattr(k8s_agent_sandbox, "SandboxClient", _FakeBaseClient)
  c = Cluster(ClusterConfig(name="plain"), api_client=MagicMock())
  assert isinstance(c.sandbox_client, _FakeBaseClient)
  assert c.sandbox_client.k8s_helper == "helper"


# ------------------------------------------------------------- review gaps


def test_cold_resume_is_counted(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  h = f.acquire(f.tasks[0])
  h.sandbox = FakeSnapSandbox()
  h.sandbox.resume = lambda wait_timeout=180: _ok(restored_from_snapshot=False,
                                                  snapshot_uid=None)
  with f._obs.run("test") as rep:
    assert f.resume(h) is False
  assert (rep.snap_restored, rep.snap_cold) == (0, 1)


def test_closed_sandbox_engine_raises_unavailable():
  h = _handle()
  h.sandbox.snapshots = None                   # the extension clears it on terminate()
  with pytest.raises(SnapshotsUnavailable, match="no snapshot engine"):
    h.snapshot()
  with pytest.raises(SnapshotsUnavailable, match="no snapshot engine"):
    h.list_snapshots()


def test_fleet_preflight_forwards_cluster_snapshot_flag(make_cluster, monkeypatch):
  seen = {}

  def capture(cluster, **kw):
    seen[cluster.name] = kw.get("snapshots")
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", capture)
  snap_c, plain_c = make_cluster("snap"), make_cluster("plain")
  snap_c.config.snapshots = True
  f = _fleet(ClusterRegistry([snap_c, plain_c]))
  f.load_tasks(["img"])
  f.preflight()
  assert seen == {"snap": True, "plain": False}


def test_recording_sets_report_around_primitives(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  assert f.report is None
  with f.recording("demo") as rep:
    h = f.acquire(f.tasks[0])
    f.release(h)
    assert f.report is rep
  assert rep.strategy == "demo" and rep.claims == 1
  assert {"claim", "release"} <= set(rep.phases)
  assert rep.total_s >= 0


async def test_async_fleet_mirrors_snapshot_wrappers(make_cluster):
  from agent_sandbox_rl import AsyncSandboxFleet
  c = make_cluster("solo")
  af = AsyncSandboxFleet(FleetConfig(), registry=ClusterRegistry([c]))
  try:
    af._fleet.load_tasks(["img"])
    h = await af.acquire(af._fleet.tasks[0], snapshot_uid="uid-a",
                         pod_annotations={"k": "v"})
    kw = c.sandbox_client.create_sandbox.call_args.kwargs
    assert kw["pod_annotations"] == {"k": "v", PIN: "uid-a"}
    h.sandbox = FakeSnapSandbox()
    with af._fleet.recording("async") as rep:
      assert await af.snapshot(h, "pre") == "uid-1"
      assert await af.suspend(h, timeout=30) == "uid-s"
      assert await af.resume(h) is True
      await af.suspend(h, snapshot=False)
      await af.restore(h, "uid-1", timeout=45)
      await af.release(h, delete_snapshots=True)
    assert h.sandbox.calls[-2:] == [("restore", "uid-1", 45), ("terminate",)]
    assert h.sandbox.snapshots.deleted_all == 1
    assert rep.snap_restored == 2
    assert {"snapshot", "suspend", "resume", "restore"} <= set(rep.phases)
    assert af.handles() == []
  finally:
    af.close()
