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

"""SandboxSession framing — unit tests with a fake websocket (no cluster)."""

import re
from unittest.mock import MagicMock

import pytest

import agent_sandbox_rl.handles as handles
from agent_sandbox_rl.handles import SandboxSession


class FakeWS:
  """Simulates a pod's bash-over-websocket: echoes the run() marker and replays
  programmed per-command output. ``outputs`` maps a command substring -> stdout."""

  def __init__(self, outputs=None):
    self.outputs = outputs or {}
    self._pending = ""
    self._open = True
    self.stdin = []

  def is_open(self):
    return self._open

  def update(self, timeout=1):
    pass

  def write_stdin(self, s):
    self.stdin.append(s)
    if not self._open:                        # closed stream: nothing echoes back
      return
    m = re.search(r'printf "%s%s\\n" "([^"]+)" "([^"]+)"', s)
    if m:                                    # the sentinel line -> emit assembled marker
      self._pending += m.group(1) + m.group(2) + "\n"
      return
    for key, out in self.outputs.items():   # a real command -> emit its programmed output
      if key in s:
        self._pending += out
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


@pytest.fixture
def patch_stream(monkeypatch):
  holder = {}
  def _factory(outputs=None):
    ws = FakeWS(outputs)
    holder["ws"] = ws
    monkeypatch.setattr(handles, "stream", lambda *a, **k: ws)
    return ws
  return _factory


def test_session_run_returns_command_output(patch_stream):
  patch_stream({"git rev-parse": "abc123\n"})
  s = SandboxSession(MagicMock(), "pod", "ns")
  out = s.run("git rev-parse HEAD")
  assert out == "abc123\n"


def test_session_strips_crlf(patch_stream):
  patch_stream({"echo": "line1\r\nline2\r\n"})
  s = SandboxSession(MagicMock(), "pod", "ns")
  assert s.run("echo x") == "line1\nline2\n"


def test_session_marker_not_matched_in_echoed_input(patch_stream):
  # even if the pod echoes the command line back, the assembled marker only
  # appears in real output -> run() returns just the output, not the echo.
  ws = patch_stream({"mycmd": "RESULT\n"})
  s = SandboxSession(MagicMock(), "pod", "ns")
  out = s.run("mycmd")
  assert out == "RESULT\n"


def test_session_run_raises_when_stream_closed(patch_stream):
  ws = patch_stream({})
  s = SandboxSession(MagicMock(), "pod", "ns")
  ws._open = False
  ws._pending = ""                           # no marker will ever arrive
  with pytest.raises(TimeoutError):
    s.run("whatever", timeout=0.2)


def test_session_close_marks_closed(patch_stream):
  ws = patch_stream({})
  s = SandboxSession(MagicMock(), "pod", "ns")
  assert s.is_open
  s.close()
  assert not s.is_open


def test_handle_exec_routes_through_session(monkeypatch):
  from agent_sandbox_rl.handles import SandboxHandle
  from agent_sandbox_rl.sources import Task
  sess = MagicMock()
  sess.is_open = True
  sess.container = None                      # bound to the pod's default container
  sess.run.return_value = "via-session"
  h = SandboxHandle(task=Task(id="t", image="i"), cluster_name="c",
                    claim_name="cl", sandbox_id="s", pod_name="p", hostname="s")
  h._session = sess
  assert h.exec(["bash", "-lc", "echo hi"]) == "via-session"
  sess.run.assert_called_once()


# ------------------------------------------------ multi-container pods


def _handle_with_cluster():
  from agent_sandbox_rl.handles import SandboxHandle
  from agent_sandbox_rl.sources import Task
  core = MagicMock()
  cluster = MagicMock()
  cluster.namespace = "ns"
  cluster.exec_core_api.return_value = core
  h = SandboxHandle(task=Task(id="t", image="i"), cluster_name="c",
                    claim_name="cl", sandbox_id="s", pod_name="p", hostname="s",
                    _cluster=cluster)
  return h, core


def _capture_stream(monkeypatch, ws_factory=None):
  calls = []
  def fake(func, pod, ns, **kw):
    calls.append((pod, ns, kw))
    return ws_factory() if ws_factory else "out"
  monkeypatch.setattr(handles, "stream", fake)
  return calls


def test_exec_in_pod_passes_the_container_only_when_set(monkeypatch):
  calls = _capture_stream(monkeypatch)
  handles.exec_in_pod(MagicMock(), "p", "ns", "echo hi")
  handles.exec_in_pod(MagicMock(), "p", "ns", "echo hi", container="eval")
  assert "container" not in calls[0][2]        # API default = first container
  assert calls[1][2]["container"] == "eval"


def test_session_binds_to_a_container(monkeypatch):
  calls = _capture_stream(monkeypatch, lambda: FakeWS({}))
  s = SandboxSession(MagicMock(), "p", "ns", container="eval")
  assert s.container == "eval" and calls[0][2]["container"] == "eval"
  s2 = SandboxSession(MagicMock(), "p", "ns")
  assert s2.container is None and "container" not in calls[1][2]


def test_handle_exec_uses_target_container_and_per_call_override(monkeypatch):
  calls = _capture_stream(monkeypatch)
  h, _ = _handle_with_cluster()
  h.exec("a")                                  # default container
  h.target_container = "eval"
  h.exec("b")                                  # handle-level target
  h.exec("c", container="agent-runtime")       # per-call override wins
  assert [c[2].get("container") for c in calls] == [None, "eval", "agent-runtime"]


def test_handle_exec_reuses_a_session_only_for_its_container(monkeypatch):
  calls = _capture_stream(monkeypatch)
  h, _ = _handle_with_cluster()
  sess = MagicMock()
  sess.is_open = True
  sess.container = "eval"
  sess.run.return_value = "via-session"
  h._session = sess
  assert h.exec("x", container="eval") == "via-session"      # matching container
  assert h.exec("y") == "out"                                # default: one-shot
  assert calls and calls[-1][2].get("container") is None
  h.target_container = "eval"
  assert h.exec("z") == "via-session"                        # target matches session
  assert sess.run.call_count == 2


def test_open_session_switches_container_by_closing_the_previous_one(monkeypatch):
  calls = _capture_stream(monkeypatch, lambda: FakeWS({}))
  h, _ = _handle_with_cluster()
  first = h.open_session()
  assert first.container is None and h.open_session() is first      # reused
  second = h.open_session(container="eval")
  assert second is not first and second.container == "eval"
  assert not first.is_open                                          # closed first
  assert calls[-1][2]["container"] == "eval"
  h.target_container = "eval"
  assert h.open_session() is second                                 # target matches


def test_as_script_shell_quotes_argv():
  # bash -lc payload passes through; other argv is shell-quoted so it round-trips
  # through the session (the sh -c script stays a single argument).
  assert handles._as_script("echo hi") == "echo hi"
  assert handles._as_script(["bash", "-lc", "echo hi"]) == "echo hi"
  assert handles._as_script(["sh", "-c", "echo $(hostname)"]) == "sh -c 'echo $(hostname)'"
