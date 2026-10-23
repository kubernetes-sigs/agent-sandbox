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

import logging
import shlex
import time
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Callable, Optional

from kubernetes import client
from kubernetes.stream import stream

from . import constants
from . import snapshots as _snap
from .exceptions import FleetError, SandboxLostError, SnapshotsUnavailable
from .sources import Task

logger = logging.getLogger("agent_sandbox_rl.handles")

if TYPE_CHECKING:
  from .cluster import Cluster

logger = logging.getLogger("agent_sandbox_rl.handles")

# refresh(): how often to try reading the new pod's name before giving up, and the
# linear backoff between tries (a just-resumed pod can lag the Sandbox status).
_REFRESH_ATTEMPTS = 3
_REFRESH_BACKOFF_S = 0.5


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


def _pod_dict(pod) -> dict:
  return pod.to_dict() if hasattr(pod, "to_dict") else (pod or {})


def _runtime_status(d: dict) -> dict | None:
  """The runtime container's status, or None if no container has that name."""
  status = d.get("status") or {}
  for c in status.get("container_statuses") or status.get("containerStatuses") or []:
    if (c or {}).get("name") == constants.RUNTIME_CONTAINER:
      return c
  return None


def _pod_identity(pod) -> tuple[Optional[str], Optional[int]]:
  """``(uid, runtime container restart count)``; either is None when unknown."""
  d = _pod_dict(pod)
  uid = (d.get("metadata") or {}).get("uid")
  rs = _runtime_status(d)
  restarts = None
  if rs is not None:
    restarts = rs.get("restart_count", rs.get("restartCount"))
  return uid, restarts


def _pod_is_live(pod, *, expected_uid: Optional[str] = None,
                 expected_restarts: Optional[int] = None) -> bool:
  """False once a pod is being deleted, has finished, its runtime container has
  terminated, or it is no longer the pod the handle was acquired on: the Sandbox
  controller recreates an evicted or deleted pod under the same name (new UID),
  and kubelet restarts an OOM-killed container in place (higher restart count,
  ``state.terminated`` cleared). Only the runtime container is judged; if none
  has that name the answer is True (unknown is not dead), so a sidecar that
  exits on its own does not count. Accepts a ``V1Pod`` or its ``to_dict()``."""
  d = _pod_dict(pod)
  meta = d.get("metadata") or {}
  status = d.get("status") or {}
  if meta.get("deletion_timestamp") or meta.get("deletionTimestamp"):
    return False
  if status.get("phase") in ("Failed", "Succeeded"):
    return False
  uid, restarts = _pod_identity(d)
  if expected_uid and uid and uid != expected_uid:
    return False
  if expected_restarts is not None and restarts is not None and restarts > expected_restarts:
    return False
  rs = _runtime_status(d)
  if rs is not None and (rs.get("state") or {}).get("terminated"):
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



def forget_claim_in_client(cluster, claim_name: str) -> None:
  """Drop a claim from the SDK client's per-process tracking after the fleet
  removed it outside ``SandboxClient.delete_sandbox``.

  ``create_sandbox`` records the Sandbox object in
  ``_active_connection_sandboxes`` and, for a caller-named claim, the name in
  ``_explicit_claims``; ``sandbox.terminate()`` and a direct claim delete leave
  both in place, so a fleet that claims per episode grows them for the life of
  the process. Best-effort over the client's private state until the SDK prunes
  it on the terminate path itself."""
  client = getattr(cluster, "sandbox_client", None)
  if client is None:
    return
  key = (cluster.namespace, claim_name)
  active = getattr(client, "_active_connection_sandboxes", None)
  if isinstance(active, dict):
    active.pop(key, None)
  explicit = getattr(client, "_explicit_claims", None)
  if isinstance(explicit, set):
    explicit.discard(key)

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

  On a cluster with ``ClusterConfig(snapshots=True)`` (GKE Pod Snapshots) the
  handle also offers ``snapshot()`` / ``suspend()`` / ``resume()`` /
  ``restore()`` / ``list_snapshots()``; a resume or restore brings up a **new
  pod**, so those calls end with ``refresh()`` (new ``pod_name`` / ``pod_ip``,
  exec session dropped). ``hostname`` / ``sandbox_id`` / ``claim_name`` are
  stable across it. ``pending_snapshot_uid`` is set when ``acquire(task,
  snapshot_uid=...)`` adopted an already-running warm pod, so the pin was not
  applied: it applies to the sandbox's next pod (suspend + resume), or call
  ``restore(uid)``; a resume or restore clears it. Prefer the fleet's
  ``suspend`` / ``resume`` / ``restore`` wrappers over the handle methods: the
  fleet gives a suspended sandbox's claim slot back to ``max_live_sandboxes``
  and takes it again on resume, which the bare handle methods do not.
  """

  task: Task
  cluster_name: str
  claim_name: str
  sandbox_id: str
  pod_name: str
  hostname: str
  pod_ip: Optional[str] = None
  sandbox: object = None
  pending_snapshot_uid: Optional[str] = None
  _cluster: "Cluster" = field(default=None, repr=False)
  _session: Optional["SandboxSession"] = field(default=None, repr=False)
  _slot_released: bool = field(default=False, repr=False)
  # The pod this handle was acquired on (see record_pod_identity): a replacement
  # pod under the same name, or an in-place restart, means the state is gone.
  pod_uid: Optional[str] = field(default=None, repr=False)
  runtime_restarts: Optional[int] = field(default=None, repr=False)

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

  def record_pod_identity(self) -> None:
    """Remember which pod this handle is on: its UID and the runtime container's
    restart count. Best-effort (one bounded ``GET``); `acquire()` calls it.
    Without it, `is_alive()` cannot tell a replacement pod from the original."""
    if self._cluster is None:
      return
    # A failed read must leave the identity unknown, not the previous pod's:
    # after a resume or restore a stale UID would read the new pod as lost.
    self.pod_uid, self.runtime_restarts = None, None
    try:
      pod = self._cluster.exec_core_api().read_namespaced_pod_status(
          self.pod_name, self._cluster.namespace, _request_timeout=(3, 10))
      self.pod_uid, self.runtime_restarts = _pod_identity(pod)
    except Exception:  # noqa: BLE001 — liveness then falls back to presence checks
      logger.debug("could not record pod identity for %s", self.pod_name,
                   exc_info=True)

  def is_alive(self) -> bool:
    """Whether the sandbox's state is still there: the pod it was acquired on
    still exists and is running.

    One ``GET`` on the pod. False if the pod is not found, is being deleted, has
    reached ``Failed``/``Succeeded``, its runtime container has terminated, or it
    is not the same pod any more (a different UID: the controller recreated an
    evicted or deleted pod under the same name; or a higher restart count: the
    runtime container restarted in place, as after an OOM kill). A handle with
    no cluster wiring answers True (unknown is not dead).

    ``exec()`` consults this only after a transport error, so a live sandbox pays
    nothing for it. A one-shot ``exec()`` against a same-name replacement pod
    succeeds without a transport error, so a loop that must not continue on lost
    state should call ``is_alive()`` at its step boundaries."""
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
    return _pod_is_live(pod, expected_uid=self.pod_uid,
                        expected_restarts=self.runtime_restarts)

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

  # --- GKE Pod Snapshots (opt-in: ClusterConfig(snapshots=True)) ----------- #
  def _snapshot_sandbox(self):
    if not _snap.capable(self.sandbox):
      raise SnapshotsUnavailable(
          f"sandbox {self.sandbox_id!r} has no snapshot support — " + _snap.HINT)
    return self.sandbox

  def _snapshot_engine(self):
    engine = getattr(self._snapshot_sandbox(), "snapshots", None)
    if engine is None:
      raise SnapshotsUnavailable(
          f"sandbox {self.sandbox_id!r} has no snapshot engine (closed?) — "
          + _snap.HINT)
    return engine

  def refresh(self) -> None:
    """Re-read pod identity after a pod restart (resume / restore).

    A resumed or restored sandbox is a **new pod**: new ``pod_name`` and
    ``pod_ip``; ``hostname`` / ``sandbox_id`` / ``claim_name`` do not change.
    Any persistent exec session was bound to the old pod, so it is dropped
    (``exec()`` falls back to one-shot; call ``open_session()`` again to
    re-attach). Safe to call at any time.

    Raises `FleetError` when the pod name cannot be read after a few tries. The
    handle then still names the previous pod, which no longer exists after a
    resume or restore, so call ``refresh()`` again before ``exec()``. On success
    the pod identity used by ``is_alive()`` is re-recorded for the new pod.
    """
    self.close_session()
    sb = self.sandbox
    if sb is None:
      return
    last_error: Optional[BaseException] = None
    for attempt in range(_REFRESH_ATTEMPTS):
      try:
        name = sb.get_pod_name()
      except Exception as e:  # noqa: BLE001 — retried, then raised below
        name, last_error = None, e
      if name:
        self.pod_name = name
        break
      if attempt + 1 < _REFRESH_ATTEMPTS:
        time.sleep(_REFRESH_BACKOFF_S * (attempt + 1))
    else:
      raise FleetError(
          f"refresh: could not read the pod name of sandbox {self.sandbox_id!r} "
          f"after {_REFRESH_ATTEMPTS} tries; the handle still names {self.pod_name!r}, "
          "which may be gone. Call refresh() again before exec().") from last_error
    try:
      self.pod_ip = sb.get_pod_ip()
    except Exception:  # noqa: BLE001
      self.pod_ip = None
    # The new pod is the sandbox's state from now on: re-baseline the identity
    # that is_alive() compares against, or every check after a resume would
    # report the restored pod as a replacement of the one that was suspended.
    self.record_pod_identity()

  def snapshot(self, name: str | None = None, *, timeout: int = 180) -> str:
    """Snapshot the running sandbox (memory + filesystem); returns the uid.

    ``name`` seeds the ``PodSnapshotManualTrigger`` name (default: the sandbox
    id). Blocks up to ``timeout`` seconds for the trigger to be processed. The
    sandbox keeps running (``postCheckpoint: resume`` in the policy).
    """
    r = _snap.check(self._snapshot_engine().create(
        name or f"snap-{self.sandbox_id}", podsnapshot_timeout=timeout), "snapshot")
    return r.snapshot_uid

  def list_snapshots(self) -> list:
    """This sandbox's snapshots, newest first (SDK ``SnapshotDetail`` objects)."""
    return list(_snap.check(self._snapshot_engine().list(), "list snapshots").snapshots)

  def delete_snapshot(self, snapshot_uid: str, *, timeout: int = 180) -> None:
    _snap.check(self._snapshot_engine().delete(snapshot_uid, timeout=timeout),
                "delete snapshot")

  def delete_snapshots(self, *, timeout: int = 180) -> None:
    """Delete every snapshot of this sandbox (they outlive the claim otherwise).

    Finds them through the live pod, so it only works while the claim exists;
    after a release use `snapshot_deleter()` with uids listed beforehand."""
    _snap.check(self._snapshot_engine().delete_all(timeout=timeout), "delete snapshots")

  def snapshot_deleter(self) -> Callable[..., None]:
    """A delete-by-uid function that keeps working after this handle is
    released. Deleting one snapshot addresses its PodSnapshot by name and needs
    no pod, but releasing the sandbox drops its snapshot engine, so bind it first."""
    engine = self._snapshot_engine()

    def delete(snapshot_uid: str, *, timeout: int = 180) -> None:
      _snap.check(engine.delete(snapshot_uid, timeout=timeout), "delete snapshot")
    return delete

  def suspend(self, *, snapshot: bool = True, timeout: int = 180) -> Optional[str]:
    """Suspend the sandbox: the pod is deleted, the claim (and identity) kept.

    With ``snapshot`` (default) the live state is captured first so ``resume()``
    brings back memory + filesystem; returns that snapshot's uid. Without it no
    new snapshot is taken, but ``resume()`` still restores the latest existing
    one, so any changes made since that snapshot are lost; it cold-boots the
    template only when the sandbox has no snapshots. To guarantee a cold boot,
    call ``delete_snapshots()`` before suspending. Quiesce the sandbox before calling
    (an in-flight command is frozen inside the snapshot). Blocks up to
    ``timeout`` seconds for the pod to terminate. The persistent exec session
    (if any) is closed first, even when the suspend then fails — ``exec()``
    falls back to one-shot.
    """
    sb = self._snapshot_sandbox()
    self.close_session()          # the stream is bound to the pod being deleted
    r = _snap.check(sb.suspend(snapshot_before_suspend=snapshot, wait_timeout=timeout),
                    "suspend")
    sr = getattr(r, "snapshot_response", None)
    return getattr(sr, "snapshot_uid", None) if sr is not None else None

  def resume(self, *, timeout: int = 180) -> bool:
    """Resume a suspended sandbox, restoring its **latest** snapshot if any.

    Returns whether it came back from a snapshot (False = cold boot because
    no snapshot exists, or it was not suspended). Waits up to ``timeout`` seconds for the new pod to be Ready,
    then ``refresh()``es this handle.
    """
    r = _snap.check(self._snapshot_sandbox().resume(wait_timeout=timeout), "resume")
    self.refresh()
    self.pending_snapshot_uid = None       # the new pod honored the pin, if any
    return bool(getattr(r, "restored_from_snapshot", False))

  def restore(self, snapshot_uid: str, *, timeout: int = 180) -> None:
    """Restore a **suspended** sandbox from one specific snapshot.

    Pins the snapshot via the claim's pod annotation, resumes, and verifies the
    pod's ``PodRestored`` condition names it (the SDK does the verification;
    a mismatch is a `SnapshotError`). Ends with ``refresh()``.
    """
    _snap.check(self._snapshot_sandbox().restore(
        snapshot_uid, sandbox_ready_timeout=timeout), "restore")
    self.refresh()
    self.pending_snapshot_uid = None

  @property
  def is_suspended(self) -> bool:
    """Whether the Sandbox's ``spec.operatingMode`` is ``Suspended``."""
    return bool(self._snapshot_sandbox().is_suspended())

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
    if self._cluster is not None:
      forget_claim_in_client(self._cluster, self.claim_name)
