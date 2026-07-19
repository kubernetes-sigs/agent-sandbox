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

"""`SandboxHandle` — what an RL framework consumes per claimed sandbox."""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Optional

from kubernetes.stream import stream

from .sources import Task

if TYPE_CHECKING:
  from .cluster import Cluster


def exec_in_pod(core_api, pod: str, namespace: str, command) -> str:
  """Run ``command`` in a pod via the Kubernetes exec API (router-free).

  ``command`` may be a list (argv) or a string (wrapped as ``bash -lc``).
  Returns combined stdout/stderr.
  """
  if isinstance(command, str):
    command = ["bash", "-lc", command]
  return stream(
      core_api.connect_get_namespaced_pod_exec,
      pod, namespace, command=command,
      stderr=True, stdin=False, stdout=True, tty=False,
      _preload_content=True)


def _as_script(command) -> str:
  """Normalize an exec `command` (str or argv) to a shell script string for a
  persistent session. ``["bash","-lc",script]`` → ``script``; other argv joined."""
  if isinstance(command, str):
    return command
  if len(command) >= 3 and command[0] in ("bash", "sh") and command[1] == "-lc":
    return command[2]
  return " ".join(command)


class SandboxSession:
  """One long-lived `bash` exec stream per sandbox — commands are piped over a
  single held-open websocket instead of a fresh `connect...pod_exec` per call.

  This is the control-plane lever for recycling: with a session, a sandbox's
  task + reset commands cost **one** apiserver exec connection for its whole
  life (O(sandboxes)) instead of one per command (O(tasks)) — which is what
  saturates the exec path at high concurrency.

  ``run(command)`` returns combined stdout (like `exec_in_pod`). Framing: a
  sentinel is emitted after each command; the sentinel's assembled form never
  appears in the echoed input (built by shell concatenation), so it matches only
  real output. A pty (`tty=True`) keeps bash line-buffered so small outputs flush
  promptly. Not thread-safe — one session is driven by one worker.
  """

  def __init__(self, core_api, pod: str, namespace: str, *,
               open_timeout: float = 30.0):
    self.pod = pod
    self._seq = 0
    self._resp = stream(
        core_api.connect_get_namespaced_pod_exec, pod, namespace,
        command=["bash"], stderr=True, stdin=True, stdout=True, tty=True,
        _preload_content=False)
    # Let the shell come up and drain the initial prompt/banner.
    deadline = time.monotonic() + open_timeout
    while time.monotonic() < deadline and not self._resp.is_open():
      self._resp.update(timeout=1)
    self.run("stty -echo 2>/dev/null; true")   # quiet the pty echo where supported

  @property
  def is_open(self) -> bool:
    try:
      return bool(self._resp.is_open())
    except Exception:  # noqa: BLE001
      return False

  def run(self, command, timeout: float = 120.0) -> str:
    self._seq += 1
    # Assemble the marker at runtime ("__A""B" -> "AB") so the literal never
    # appears in the pty-echoed command line — only in the command's output.
    tok = f"ASRLDONE{self._seq}X"
    head, tail = tok[:6], tok[6:]
    script = _as_script(command)
    self._resp.write_stdin(script + "\n")
    self._resp.write_stdin(f'printf "%s%s\\n" "{head}" "{tail}"\n')
    marker = tok
    buf = []
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
      self._resp.update(timeout=1)
      if self._resp.peek_stdout():
        buf.append(self._resp.read_stdout())
        text = "".join(buf)
        if marker in text:
          out = text.split(marker, 1)[0]
          return out.replace("\r\n", "\n")
      if self._resp.peek_stderr():
        self._resp.read_stderr()               # drain to avoid backpressure
      if not self.is_open:
        break
    raise TimeoutError(f"session.run timed out/closed on {self.pod}")

  def close(self) -> None:
    try:
      self._resp.write_stdin("exit\n")
    except Exception:  # noqa: BLE001
      pass
    try:
      self._resp.close()
    except Exception:  # noqa: BLE001
      pass


@dataclass
class SandboxHandle:
  """A claimed sandbox bound to one task on one cluster.

  Attributes:
    task: The `Task` this sandbox serves.
    cluster_name: Name of the owning cluster.
    claim_name: The SandboxClaim name (delete to release).
    sandbox_id: The Sandbox resource name = its **stable in-cluster hostname**.
    pod_name: Backing pod name (for ``kubectl exec``).
    hostname: Stable in-cluster DNS name (== ``sandbox_id``).
    pod_ip: Pod IP if known.
    sandbox: The underlying SDK ``Sandbox`` (``.commands`` / ``.files`` — needs
      the Sandbox Router; ``exec()`` below is the router-free path).
  """

  task: Task
  cluster_name: str
  claim_name: str
  sandbox_id: str
  pod_name: str
  hostname: str
  pod_ip: Optional[str] = None
  sandbox: object = None
  _cluster: "Cluster" = field(default=None, repr=False)
  _session: Optional["SandboxSession"] = field(default=None, repr=False)

  def exec(self, command) -> str:
    """Run a command inside the sandbox (router-free, via the pod's exec API).

    If a persistent `SandboxSession` is attached (``open_session()``), the command
    is piped over that single held-open stream — no per-command websocket connect
    (the recycling control-plane lever). Otherwise a fresh one-shot exec is used,
    via a **thread-local** ``CoreV1Api`` (``Cluster.exec_core_api``): the
    kubernetes ``stream()`` websocket exec is not thread-safe across a shared
    client, so parallel one-shot execs stay isolated per thread while the client
    is cached per thread rather than rebuilt per call.
    """
    if self._session is not None and self._session.is_open:
      return self._session.run(command)
    core = self._cluster.exec_core_api()
    return exec_in_pod(core, self.pod_name, self._cluster.namespace, command)

  def open_session(self) -> "SandboxSession":
    """Open (once) a persistent exec session so subsequent ``exec()`` calls reuse
    a single websocket instead of connecting per command. Returns it."""
    if self._session is None or not self._session.is_open:
      self._session = SandboxSession(
          self._cluster.exec_core_api(), self.pod_name, self._cluster.namespace)
    return self._session

  def close_session(self) -> None:
    if self._session is not None:
      self._session.close()
      self._session = None

  def endpoint(self, port: int = 8888) -> str:
    """In-cluster endpoint (``<hostname>.<namespace>:<port>``) for callers that
    reach the sandbox over the network rather than via exec."""
    return f"{self.hostname}.{self._cluster.namespace}:{port}"

  def release(self) -> None:
    """Release this sandbox (delete its claim).

    Note: when managed by a `SandboxFleet`, prefer ``fleet.release(handle)`` —
    it also updates the fleet's claim/replica bookkeeping under its lock. Calling
    this directly just frees the remote resources.
    """
    self.close_session()                       # drop the persistent stream, if any
    if self.sandbox is not None:
      self.sandbox.terminate()
    else:
      self._cluster.sandbox_client.delete_sandbox(
          self.claim_name, namespace=self._cluster.namespace)
