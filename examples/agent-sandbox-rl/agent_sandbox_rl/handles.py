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

import shlex
import time
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Optional

from kubernetes import client
from kubernetes.stream import stream

from . import constants
from .exceptions import SandboxLostError
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
  out = stream(
      core_api.connect_get_namespaced_pod_exec,
      pod, namespace, command=command,
      stderr=True, stdin=False, stdout=True, tty=False,
      _preload_content=True)
  if out is None:
    return ""
  if isinstance(out, bytes):
    return out.decode("utf-8", errors="replace")
  return str(out)


def _as_script(command) -> str:
  """Normalize an exec `command` (str or argv) to a shell script string for a
  persistent session. ``["bash","-lc",script]`` → ``script``; other argv joined."""
  if isinstance(command, str):
    return command
  if len(command) >= 3 and command[0] in ("bash", "sh") and command[1] == "-lc":
    return command[2]
  # shell-quote each arg so argv round-trips through the bash session intact
  # (e.g. ['sh','-c','echo $(hostname)'] -> "sh -c 'echo $(hostname)'").
  return shlex.join(command)


def _pod_is_live(pod) -> bool:
  """False once a pod is being deleted, has finished, or its runtime container
  has terminated. Accepts a ``V1Pod`` or its ``to_dict()``."""
  d = pod.to_dict() if hasattr(pod, "to_dict") else (pod or {})
  meta = d.get("metadata") or {}
  status = d.get("status") or {}
  if meta.get("deletion_timestamp") or meta.get("deletionTimestamp"):
    return False
  if status.get("phase") in ("Failed", "Succeeded"):
    return False
  statuses = status.get("container_statuses") or status.get("containerStatuses") or []
  runtime = [c for c in statuses if (c or {}).get("name") == constants.RUNTIME_CONTAINER]
  for c in runtime or statuses:
    if ((c or {}).get("state") or {}).get("terminated"):
      return False
  return True


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
        command=["bash", "-l"], stderr=True, stdin=True, stdout=True, tty=True,
        _preload_content=False)
    # Let the shell come up and drain the initial prompt/banner.
    deadline = time.monotonic() + open_timeout
    while time.monotonic() < deadline and not self._resp.is_open():
      self._resp.update(timeout=1)
    if not self.is_open:                        # honor open_timeout explicitly
      raise TimeoutError(
          f"session websocket did not open within {open_timeout}s on {pod}")
    # `bash -l` matches the one-shot `bash -lc` path (sources /etc/profile* where
    # conda activation lives) so a session and a fallback one-shot share the env.
    # Quiet the pty echo and clear PS1/PROMPT_COMMAND so no prompt/banner leaks
    # into command output.
    self.run("stty -echo 2>/dev/null; PS1=''; PROMPT_COMMAND=''; true",
             timeout=open_timeout)

  @property
  def is_open(self) -> bool:
    try:
      return bool(self._resp.is_open())
    except Exception:  # noqa: BLE001
      return False

  def run(self, command, timeout: float | None = None) -> str:
    """Run ``command`` over the session, returning combined stdout/stderr.

    ``timeout=None`` waits indefinitely (parity with the one-shot exec path, whose
    ``_preload_content=True`` blocks until the command finishes) — so attaching a
    session never silently caps a command. On timeout or a closed socket the session
    is closed before raising, so its stale buffer can't bleed into the next call."""
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
    deadline = None if timeout is None else time.monotonic() + timeout
    while True:
      if deadline is not None:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
          break
        upd = min(1.0, remaining)
      else:
        upd = 1.0
      self._resp.update(timeout=upd)             # honor small timeouts
      if self._resp.peek_stderr():
        buf.append(self._resp.read_stderr())   # combine stderr (exec() returns both)
      if self._resp.peek_stdout():
        buf.append(self._resp.read_stdout())
        text = "".join(buf)
        if marker in text:
          out = text.split(marker, 1)[0]
          return out.replace("\r\n", "\n")
      if not self.is_open:
        break
    self.close()                                 # drop stale state; never reuse a timed-out session
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

  def exec(self, command, timeout: float | None = None) -> str:
    """Run a command inside the sandbox (router-free, via the pod's exec API).

    ``timeout`` (seconds) bounds the wait on the **session** path; ``None`` (default)
    waits until the command finishes. The one-shot path always runs to completion
    (``_preload_content=True`` blocks), so a finite ``timeout`` takes effect only when
    a session is attached — the point being that attaching a session no longer
    silently caps a command (the previous 120s default).

    If a persistent `SandboxSession` is attached (``open_session()``), the command
    is piped over that single held-open stream — no per-command websocket connect
    (the recycling control-plane lever). Otherwise a fresh one-shot exec is used,
    via a **thread-local** ``CoreV1Api`` (``Cluster.exec_core_api``): the
    kubernetes ``stream()`` websocket exec is not thread-safe across a shared
    client, so parallel one-shot execs stay isolated per thread while the client
    is cached per thread rather than rebuilt per call.
    """
    try:
      if self._session is not None and self._session.is_open:
        return self._session.run(command, timeout=timeout)
      core = self._cluster.exec_core_api()
      return exec_in_pod(core, self.pod_name, self._cluster.namespace, command)
    except Exception as e:  # noqa: BLE001 — re-raised below unless the pod is gone
      self._raise_if_lost(e)
      raise

  def is_alive(self) -> bool:
    """Whether the backing pod still exists and has not finished.

    One ``GET`` on the pod. False if the pod is not found, is being deleted, has
    reached ``Failed``/``Succeeded``, or its runtime container has terminated.
    A handle with no cluster wiring answers True (unknown is not dead).
    ``exec()`` consults this only after a transport error, so a live sandbox
    pays nothing for it."""
    if self._cluster is None:
      return True
    try:
      # Bounded (connect, read): this runs on an error path and must not turn a
      # stalled API server into a hang; on timeout the original error stands.
      pod = self._cluster.exec_core_api().read_namespaced_pod_status(
          self.pod_name, self._cluster.namespace, _request_timeout=(3, 10))
    except client.ApiException as e:
      if e.status == 404:
        return False
      raise
    return _pod_is_live(pod)

  def _raise_if_lost(self, cause: BaseException) -> None:
    """Raise `SandboxLostError` when an exec failure is explained by a dead pod.

    A pod that was OOM-killed or evicted mid-episode surfaces as a 404 on the
    exec handshake or as a closed session (``TimeoutError``). Both are also what
    a slow command looks like, so the pod is checked before deciding. If the
    check itself fails the original error stands."""
    try:
      alive = self.is_alive()
    except Exception:  # noqa: BLE001
      return
    if not alive:
      self.close_session()
      raise SandboxLostError(self.pod_name, self.claim_name, cause) from cause

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
