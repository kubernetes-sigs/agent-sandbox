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

"""Regression tests for the PR #1232 review fixes:
- Sandbox list/delete use the CORE API group/version (not extensions).
- The run-id / managed labels propagate to the pod template (so pods carry them).
- Resources.count_pods; the circuit breaker counts pods (run-id label lives there).
- Drift tripwire quarantines instead of silently skipping on a missing baseline.
- SandboxSession: login shell, unbounded default timeout, close-on-timeout.
"""

import re
from unittest.mock import MagicMock

import pytest
from kubernetes import client

import agent_sandbox_rl.handles as handles
from agent_sandbox_rl import (ClusterRegistry, FleetConfig, GitRestoreReset,
                              SandboxFleet, constants)
from agent_sandbox_rl.config import TemplateSpec
from agent_sandbox_rl.handles import SandboxSession
from agent_sandbox_rl.resources import Resources

IMG = "reg/swebench-verified@sha256:deadbeef"
TNAME = "r2e-img-abc123"


def _res(labels=None):
  return Resources(MagicMock(), MagicMock(), "ns", labels=labels)


# --- #3: Sandbox is in the CORE group, not extensions --------------------- #
def test_list_sandboxes_uses_core_group():
  r = _res()
  r.custom_api.list_namespaced_custom_object.return_value = {"items": []}
  r.list_sandboxes(label_selector="x=y")
  _, kw = r.custom_api.list_namespaced_custom_object.call_args
  assert kw["group"] == constants.SANDBOX_GROUP
  assert kw["version"] == constants.SANDBOX_VERSION
  assert kw["plural"] == constants.SANDBOXES_PLURAL


def test_delete_sandbox_uses_core_group():
  r = _res()
  r.delete_sandbox("sb-1")
  _, kw = r.custom_api.delete_namespaced_custom_object.call_args
  assert kw["group"] == constants.SANDBOX_GROUP
  assert kw["version"] == constants.SANDBOX_VERSION


def test_extensions_resources_still_use_extensions_group():
  r = _res()
  r.custom_api.list_namespaced_custom_object.return_value = {"items": []}
  r.list_warmpools()
  _, kw = r.custom_api.list_namespaced_custom_object.call_args
  assert kw["group"] == constants.GROUP  # unchanged for warmpools/templates/claims


# --- #1/#2: run-id label propagates to the pod template (pods carry it) ---- #
def test_pod_template_carries_run_and_managed_labels():
  r = _res(labels={constants.RUN_ID_LABEL: "run123"})
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  r.ensure_template(IMG, TNAME, TemplateSpec())
  _, kw = r.custom_api.create_namespaced_custom_object.call_args
  labels = kw["body"]["spec"]["podTemplate"]["metadata"]["labels"]
  assert labels["sandbox"] == TNAME                                   # colocation label kept
  assert labels[constants.RUN_ID_LABEL] == "run123"                   # so pods are run-scoped
  assert labels[constants.MANAGED_BY_LABEL] == constants.MANAGED_BY_VALUE


# --- #1: count_pods + breaker counts pods --------------------------------- #
def test_count_pods_uses_remaining_item_count():
  r = _res()
  resp = MagicMock(items=[object()])            # only 1 object transferred
  resp.metadata.remaining_item_count = 41
  r.core_api.list_namespaced_pod.return_value = resp
  assert r.count_pods(label_selector="run=x") == 42   # 1 + 41, no full list
  _, kw = r.core_api.list_namespaced_pod.call_args
  assert kw["label_selector"] == "run=x" and kw["limit"] == 1


def test_count_pods_small_set_single_page():
  r = _res()
  resp = MagicMock(items=[object()])
  resp.metadata.remaining_item_count = None
  resp.metadata._continue = None                # whole set fit in the first page
  r.core_api.list_namespaced_pod.return_value = resp
  assert r.count_pods() == 1


def test_count_pods_fallback_full_list_when_no_hint():
  r = _res()
  page = MagicMock(items=[object()])
  page.metadata.remaining_item_count = None
  page.metadata._continue = "tok"               # more pages, but server gave no count hint
  full = MagicMock(items=[object(), object(), object()])
  r.core_api.list_namespaced_pod.side_effect = [page, full]
  assert r.count_pods() == 3


def test_live_owned_count_counts_pods_by_run_id(make_cluster):
  c = make_cluster("solo")
  c.resources.count_pods.return_value = 7
  f = SandboxFleet(FleetConfig(), registry=ClusterRegistry([c]))
  assert f.live_owned_count() == 7
  _, kw = c.resources.count_pods.call_args
  assert kw["label_selector"] == f.run_selector()   # keyed off run-id, which pods carry


# --- #9: enabled tripwire with a missing baseline quarantines (not skip) --- #
class _FakeHandle:
  def __init__(self, fps):
    self.cluster_name = "c"; self.pod_name = "p"; self._fps = list(fps); self.calls = []

  def exec(self, command):
    self.calls.append(command)
    return self._fps.pop(0)


@pytest.mark.parametrize("fp,reason", [
    ("PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=\nCFG=g1\nPROCS=5", "env_baseline_missing"),
    ("PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e1\nCFG=\nPROCS=5", "config_baseline_missing"),
])
def test_missing_baseline_hash_quarantines(fp, reason):
  h = _FakeHandle([fp, fp])                      # prime, then reset (nothing drifts)
  r = GitRestoreReset(check_env=True, check_config=True)
  base = r.prime(h)
  out = r.reset(h, base)
  assert not out.clean and out.reason == reason  # can't verify → not "clean"


# --- #5/#6/#8: SandboxSession shell + timeout semantics ------------------- #
class _FakeWS:
  def __init__(self, outputs=None, emit_marker=True):
    self.outputs = outputs or {}; self._pending = ""; self._open = True
    self.emit_marker = emit_marker

  def is_open(self):
    return self._open

  def update(self, timeout=1):
    pass

  def write_stdin(self, s):
    if not self._open:
      return
    m = re.search(r'printf "%s%s\\n" "([^"]+)" "([^"]+)"', s)
    if m:
      if self.emit_marker:
        self._pending += m.group(1) + m.group(2) + "\n"
      return
    for k, o in self.outputs.items():
      if k in s:
        self._pending += o
        break

  def peek_stdout(self):
    return len(self._pending)

  def read_stdout(self):
    p, self._pending = self._pending, ""
    return p

  def peek_stderr(self):
    return 0

  def read_stderr(self):
    return ""

  def close(self):
    self._open = False


def test_session_uses_login_shell(monkeypatch):
  captured = {}

  def fake_stream(*a, command=None, **k):
    captured["command"] = command
    return _FakeWS()

  monkeypatch.setattr(handles, "stream", fake_stream)
  SandboxSession(MagicMock(), "pod", "ns")
  assert captured["command"] == ["bash", "-l"]   # matches one-shot `bash -lc` env


def test_session_run_unbounded_default_returns(monkeypatch):
  ws = _FakeWS(outputs={"echo hi": "hi\n"})
  monkeypatch.setattr(handles, "stream", lambda *a, **k: ws)
  s = SandboxSession(MagicMock(), "pod", "ns")
  assert s.run("echo hi").strip() == "hi"        # timeout=None default, no silent cap


def test_session_run_timeout_closes_session(monkeypatch):
  ws = _FakeWS(outputs={"echo hi": "hi\n"})
  monkeypatch.setattr(handles, "stream", lambda *a, **k: ws)
  s = SandboxSession(MagicMock(), "pod", "ns")    # init completes (marker on)
  ws.emit_marker = False                          # next command never completes
  with pytest.raises(TimeoutError):
    s.run("sleep 999", timeout=0.05)
  assert not s.is_open                            # timed-out session is dropped, not reused
