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
  sess.run.return_value = "via-session"
  h = SandboxHandle(task=Task(id="t", image="i"), cluster_name="c",
                    claim_name="cl", sandbox_id="s", pod_name="p", hostname="s")
  h._session = sess
  assert h.exec(["bash", "-lc", "echo hi"]) == "via-session"
  sess.run.assert_called_once()
