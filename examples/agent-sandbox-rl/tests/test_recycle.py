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

"""Recycling / git-restore reset — unit tests (no cluster; fake exec)."""

import pytest

from agent_sandbox_rl import (ClusterRegistry, FleetConfig, GitRestoreReset,
                              SandboxFleet, determinism_canary,
                              reuse_git_restore_sandbox)
from agent_sandbox_rl.handles import SandboxHandle
from agent_sandbox_rl.preflight import PreflightReport


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


# --- GitRestoreReset against a scripted fake handle ----------------------- #
class FakeHandle:
  """Records exec() calls and replays canned fingerprint lines per call."""

  def __init__(self, fingerprints):
    self.cluster_name = "c"
    self.pod_name = "pod-x"
    self._fps = list(fingerprints)
    self.calls = []

  def exec(self, command):
    self.calls.append(command)
    return self._fps.pop(0)


_CLEAN = ("PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e1\nCFG=g1\nPROCS=5")


def test_prime_captures_baseline_and_disables_gc():
  h = FakeHandle([_CLEAN])
  base = GitRestoreReset().prime(h)
  assert base.pristine_sha == "abc"
  assert base.env_hash == "e1"
  assert base.config_hash == "g1"
  assert base.proc_count == 5
  script = h.calls[0][2]
  assert "gc.auto 0" in script
  assert "maintenance.auto false" in script
  assert "gc.pruneExpire never" in script
  assert "tag -f pristine" in script


def test_reset_clean_when_fingerprint_matches():
  h = FakeHandle([_CLEAN, _CLEAN])       # prime, then reset
  r = GitRestoreReset()
  base = r.prime(h)
  out = r.reset(h, base)
  assert out.clean and out.reason == ""
  script = h.calls[1][2]
  assert "reset -q --hard pristine" in script
  assert "clean -qxdff" in script


@pytest.mark.parametrize("fp,reason", [
    ("PRISTINE=abc\nHEAD=xyz\nDIRTY=0\nENV=e1\nCFG=g1\nPROCS=5", "head_mismatch"),
    ("PRISTINE=abc\nHEAD=abc\nDIRTY=3\nENV=e1\nCFG=g1\nPROCS=5", "worktree_dirty"),
    ("PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e2\nCFG=g1\nPROCS=5", "env_drift"),
    ("PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e1\nCFG=g2\nPROCS=5", "config_or_hooks_drift"),
    ("PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e1\nCFG=g1\nPROCS=99", "process_leak"),
])
def test_reset_detects_each_pollution_vector(fp, reason):
  h = FakeHandle([_CLEAN, fp])
  # enable every tripwire (defaults are off = git-only fast path)
  r = GitRestoreReset(check_env=True, check_config=True)
  base = r.prime(h)
  out = r.reset(h, base)
  assert not out.clean
  assert out.reason == reason


def test_git_only_default_ignores_env_and_config_drift():
  # defaults check_env/check_config off: only git state is verified
  drift = "PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e2\nCFG=g2\nPROCS=5"
  h = FakeHandle([_CLEAN, drift])
  r = GitRestoreReset()
  base = r.prime(h)
  assert r.reset(h, base).clean            # env+config drift ignored by default


def test_git_only_fingerprint_omits_pip_freeze():
  # the expensive pip freeze must not be in the default (git-only) reset script
  h = FakeHandle([_CLEAN, _CLEAN])
  r = GitRestoreReset()
  base = r.prime(h)
  r.reset(h, base)
  assert all("pip freeze" not in c[2] for c in h.calls)


def test_no_pristine_anchor_is_never_clean():
  # non-git /testbed (or `git tag` failed): prime yields empty PRISTINE/HEAD ->
  # reset must refuse to claim clean, so the sandbox is quarantined not reused.
  empty = "PRISTINE=\nHEAD=\nDIRTY=0\nENV=e1\nCFG=g1\nPROCS=5"
  h = FakeHandle([empty, empty])
  r = GitRestoreReset()
  base = r.prime(h)
  out = r.reset(h, base)
  assert not out.clean
  assert out.reason == "no_pristine_anchor"


def test_env_check_can_be_disabled():
  drift = "PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e2\nCFG=g1\nPROCS=5"
  h = FakeHandle([_CLEAN, drift])
  r = GitRestoreReset(check_env=False)
  base = r.prime(h)
  assert r.reset(h, base).clean          # env drift ignored when check_env=False


# --- executor against the FakeCluster fleet ------------------------------- #
def _fleet(registry, **cfg):
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def _patch_clean_exec(monkeypatch):
  """Make every SandboxHandle.exec return a clean fingerprint (reset always OK)."""
  monkeypatch.setattr(SandboxHandle, "exec", lambda self, cmd: _CLEAN)


def test_reuse_one_claim_per_image(make_cluster, monkeypatch):
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["img", "img", "img", "img"])   # 4 tasks, 1 image
  f.setup()
  res = reuse_git_restore_sandbox(f, f.tasks, lambda t, h: h.pod_name, concurrency=4, use_session=False)
  assert len(res) == 4 and all(isinstance(x, str) for x in res)
  # one image -> one claim reused across all 4 tasks (÷G economics)
  assert c.sandbox_client.create_sandbox.call_count == 1
  f.teardown()


def test_reset_failure_triggers_quarantine(make_cluster, monkeypatch):
  # prime returns clean; every reset returns a DIRTY worktree (git-only catches
  # it regardless of env/config checks) -> quarantine each time
  drift = "PRISTINE=abc\nHEAD=abc\nDIRTY=7\nENV=e1\nCFG=g1\nPROCS=5"

  def fake_exec(self, cmd):
    # prime issues `git tag -f pristine`; resets don't
    return _CLEAN if "tag -f pristine" in cmd[2] else drift
  monkeypatch.setattr(SandboxHandle, "exec", fake_exec)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=1)
  f.load_tasks(["img", "img", "img"])          # 3 tasks, 1 image
  f.setup()
  res = reuse_git_restore_sandbox(f, f.tasks, lambda t, h: h.pod_name, concurrency=1, use_session=False)
  assert len(res) == 3
  # every reset dirty -> a fresh claim per task = 3 claims (no successful reuse)
  assert c.sandbox_client.create_sandbox.call_count == 3
  f.teardown()


def test_max_reuses_rotates_sandbox(make_cluster, monkeypatch):
  _patch_clean_exec(monkeypatch)               # resets always clean
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=1)
  f.load_tasks(["img"] * 5)                     # 5 tasks, 1 image
  f.setup()
  reuse_git_restore_sandbox(f, f.tasks, lambda t, h: 1, concurrency=1, max_reuses=2, use_session=False)
  # rotate after every 2 reuses: claims at task 1, 3, 5 -> 3 claims
  assert c.sandbox_client.create_sandbox.call_count == 3
  f.teardown()


def test_recyclable_helper():
  r = GitRestoreReset()
  from agent_sandbox_rl import ResetBaseline
  assert r.recyclable(ResetBaseline(pristine_sha="abc")) is True
  assert r.recyclable(ResetBaseline(pristine_sha="")) is False


def test_prime_exec_error_is_non_recyclable():
  class Boom:
    cluster_name = "c"
    pod_name = "p"
    def exec(self, cmd):
      raise RuntimeError("no bash")
  base = GitRestoreReset().prime(Boom())
  assert base.pristine_sha == ""            # empty -> recyclable() False


def test_reset_exec_error_quarantines_not_raises():
  class Boom:
    cluster_name = "c"
    pod_name = "p"
    calls = 0
    def exec(self, cmd):
      Boom.calls += 1
      if Boom.calls == 1:
        return _CLEAN                        # prime ok
      raise RuntimeError("pod died")         # reset exec fails
  h = Boom()
  r = GitRestoreReset()
  base = r.prime(h)
  out = r.reset(h, base)                     # must NOT raise
  assert not out.clean and out.reason == "exec_error"


def test_non_git_image_falls_back_to_fresh_per_task(make_cluster, monkeypatch):
  # empty pristine -> not recyclable -> fresh claim per task, no reset attempts
  empty = "PRISTINE=\nHEAD=\nDIRTY=0\nENV=e1\nCFG=g1\nPROCS=5"
  monkeypatch.setattr(SandboxHandle, "exec", lambda self, cmd: empty)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=1)
  f.load_tasks(["img", "img", "img"])          # 3 tasks, 1 non-git image
  f.setup()
  res = reuse_git_restore_sandbox(f, f.tasks, lambda t, h: 1, concurrency=1, use_session=False)
  assert res == [1, 1, 1]
  # non-recyclable -> a fresh claim per task = 3 claims (degrades to the regular path)
  assert c.sandbox_client.create_sandbox.call_count == 3
  f.teardown()


def test_determinism_canary_identical(make_cluster, monkeypatch):
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=1)
  f.load_tasks(["img"])
  f.setup()
  out = determinism_canary(f, f.tasks[0], lambda t, h: "same-output")
  assert out["identical"] is True
  assert out["reset_clean"] is True
  f.teardown()
