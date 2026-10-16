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

"""Fail-fast watchdog for `SandboxFleet.acquire`.

The SDK's claim wait (`SandboxClient.create_sandbox`) watches the SandboxClaim
and returns when its Ready condition flips. It already fails fast on what the
claim itself can express: the claim was deleted, its template or pool is
missing, the controller reported a terminal reason. It cannot see the pod. A
claim whose pod sits in ``ImagePullBackOff`` or ``Unschedulable`` looks exactly
like one that is still coming up, and the wait runs out ``ready_timeout``
(15 minutes by default) before the caller learns anything.

`ClaimWatchdog` closes that gap from the fleet side without touching the SDK.
`acquire` names the claim itself, starts a watchdog on that name and blocks in
the SDK wait as before. The watchdog resolves the claim's pod and polls its
status; when `classify_pod` returns a verdict it records it and deletes the
claim, which ends the SDK wait with a "claim was deleted" error that `acquire`
converts into `SandboxStartError` carrying the real reason.

Cost: one small read per poll per in-flight claim, and nothing once the claim
is Ready (the watchdog is stopped on the success path). Warm adoptions are
Ready long before ``initial_delay_s`` elapses, so only cold starts are polled.
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

from kubernetes import client

from . import constants

if TYPE_CHECKING:
  from .cluster import Cluster
  from .config import FailFastPolicy

logger = logging.getLogger(__name__)

# (connect, read) seconds for the watchdog's reads: a stalled API server must
# not pin a poll, and the next poll retries anyway.
_REQUEST_TIMEOUT = (3, 10)


@dataclass(frozen=True)
class StartVerdict:
  """Why a pod will not start. ``reason`` is the Kubernetes reason string."""
  reason: str
  message: str
  pod_name: str | None = None


def _as_dict(obj: Any) -> dict:
  """Accept a kubernetes model (``to_dict()``, snake_case keys) or a plain dict."""
  if obj is None:
    return {}
  if isinstance(obj, dict):
    return obj
  to_dict = getattr(obj, "to_dict", None)
  return to_dict() if callable(to_dict) else {}


def _get(d: dict, snake: str, camel: str):
  return d.get(snake) if snake in d else d.get(camel)


def classify_pod(pod: Any, *, now: float, first_seen: dict[str, float],
                 policy: "FailFastPolicy",
                 last_seen: dict[str, float] | None = None) -> StartVerdict | None:
  """Decide whether ``pod`` (a ``V1Pod``, its ``to_dict()`` or an API dict) is stuck.

  Pure apart from ``first_seen`` and ``last_seen``, which the caller keeps
  between polls: they map a condition key to the monotonic time it was first
  and last observed, so retryable reasons only count once they have persisted
  for the policy's grace period. All retryable waiting reasons of a container
  share one key: kubelet flips a failing pull between ``ErrImagePull`` and
  ``ImagePullBackOff`` on every retry, and that must not restart the grace. A
  key is dropped only after its condition has been absent for a full grace
  period, so a crash loop seen while the container is briefly running between
  restarts keeps its grace too. Without ``last_seen`` a key is dropped as soon
  as its condition is absent. Returns None while the pod may still come up.
  """
  if last_seen is None:
    last_seen = {}
  d = _as_dict(pod)
  meta = _as_dict(d.get("metadata"))
  status = _as_dict(d.get("status"))
  name = meta.get("name")
  present: set[str] = set()

  def _held_for(key: str, grace: float) -> float | None:
    present.add(key)
    first = first_seen.setdefault(key, now)
    last_seen[key] = now
    held = now - first
    return held if held >= grace else None

  verdict: StartVerdict | None = None
  if _get(meta, "deletion_timestamp", "deletionTimestamp"):
    verdict = StartVerdict("PodTerminating", "pod is being deleted", name)
  phase = status.get("phase")
  if verdict is None and phase in ("Failed", "Succeeded"):
    verdict = StartVerdict(
        f"Pod{phase}", status.get("message") or status.get("reason")
        or f"pod phase is {phase} before it became ready", name)

  statuses = list(_get(status, "init_container_statuses", "initContainerStatuses") or [])
  statuses += list(_get(status, "container_statuses", "containerStatuses") or [])
  for cs in statuses:
    if verdict is not None:
      break
    cs = _as_dict(cs)
    cname = cs.get("name") or "?"
    state = _as_dict(cs.get("state"))
    term = _as_dict(state.get("terminated"))
    last = _as_dict(_as_dict(_get(cs, "last_state", "lastState")).get("terminated"))
    # lastState describes the previous instance: an earlier OOM kill counts only
    # while the container is not running again. Test the value, not the key:
    # a V1Pod's to_dict() carries every state key, with None for the unset ones.
    if (term.get("reason") == "OOMKilled"
        or (last.get("reason") == "OOMKilled" and not state.get("running"))):
      verdict = StartVerdict(
          "OOMKilled", f"container '{cname}' was OOM-killed while starting; "
          "the template's memory limit is too small for this image", name)
      break
    waiting = _as_dict(state.get("waiting"))
    reason = waiting.get("reason")
    if not reason:
      continue
    msg = waiting.get("message") or reason
    if reason in constants.TERMINAL_WAITING_REASONS:
      verdict = StartVerdict(reason, f"container '{cname}': {msg}", name)
    elif reason in constants.RETRYABLE_WAITING_REASONS:
      held = _held_for(f"{cname}:retryable", policy.grace_s)
      if held is not None:
        verdict = StartVerdict(
            reason, f"container '{cname}' has been in {reason} for {int(held)}s: {msg}",
            name)

  if verdict is None:
    for cond in status.get("conditions") or []:
      cond = _as_dict(cond)
      if (cond.get("type") == "PodScheduled" and cond.get("status") == "False"
          and cond.get("reason") == "Unschedulable"):
        held = _held_for("Unschedulable", policy.unschedulable_grace_s)
        if held is not None:
          verdict = StartVerdict(
              "Unschedulable",
              f"pod unschedulable for {int(held)}s: {cond.get('message') or ''}".rstrip(": "),
              name)
        break

  for key in list(first_seen):
    if key in present:
      continue
    grace = (policy.unschedulable_grace_s if key == "Unschedulable"
             else policy.grace_s)
    if now - last_seen.get(key, float("-inf")) >= grace:
      del first_seen[key]
      last_seen.pop(key, None)
  return verdict


class ClaimWatchdog(threading.Thread):
  """Poll a claim's pod while `acquire` waits on the claim; on a terminal state
  record the verdict and delete the claim so the wait ends now.

  Best-effort by design: every API error is swallowed at debug level and the
  next poll retries, because a watchdog that raises would take a healthy claim
  down with it. ``stop()`` is cheap (sets a flag, no join) and returns the
  verdict if one was reached.
  """

  def __init__(self, cluster: "Cluster", claim_name: str, policy: "FailFastPolicy"):
    super().__init__(name=f"asrl-failfast-{claim_name}", daemon=True)
    self.cluster = cluster
    self.claim_name = claim_name
    self.policy = policy
    self.verdict: StartVerdict | None = None
    self.pod_name: str | None = None
    self._stop_evt = threading.Event()
    # Serialises the stop-versus-delete decision. `stop()` and the verdict path
    # both take it, so once `stop()` has returned the watchdog can no longer
    # delete the claim, and a verdict committed first is what `stop()` returns.
    self._decide_lock = threading.Lock()
    self.deleted_claim = False
    self._first_seen: dict[str, float] = {}
    self._last_seen: dict[str, float] = {}

  def stop(self) -> StartVerdict | None:
    """Stop polling. Blocks while a verdict is being committed, so on return
    either ``verdict`` is set and the claim delete has been attempted
    (``deleted_claim`` says whether it succeeded), or no delete will happen."""
    with self._decide_lock:
      self._stop_evt.set()
      return self.verdict

  def run(self) -> None:
    if self._stop_evt.wait(self.policy.initial_delay_s):
      return
    while not self._stop_evt.is_set():
      verdict = None
      try:
        verdict = self._check()
      except Exception:  # noqa: BLE001 — best-effort; never take the claim down
        logger.debug("fail-fast watchdog: check failed for claim %s",
                     self.claim_name, exc_info=True)
      if verdict is not None:
        with self._decide_lock:
          if self._stop_evt.is_set():
            return                       # acquire() already owns the outcome
          self.verdict = verdict
          logger.error("claim %s will not start (%s on pod %s): %s; deleting the claim",
                       self.claim_name, verdict.reason, verdict.pod_name, verdict.message)
          try:
            # Bounded: this runs under the decision lock that `stop()` takes,
            # so an unbounded delete would pin acquire()'s success path too.
            self.cluster.resources.delete_claim(
                self.claim_name, request_timeout=_REQUEST_TIMEOUT)
            self.deleted_claim = True
          except Exception:  # noqa: BLE001
            logger.warning("fail-fast watchdog: failed to delete claim %s",
                           self.claim_name, exc_info=True)
        return
      if self._stop_evt.wait(self.policy.poll_s):
        return

  # --- one poll ---------------------------------------------------------- #
  def _check(self) -> StartVerdict | None:
    if self.pod_name is None:
      self.pod_name = self._resolve_pod_name()
      if self.pod_name is None:
        return None                      # claim not bound to a sandbox yet
    pod = self._read_pod(self.pod_name)
    if pod is None:
      return None                        # pod not created yet; the claim watch owns "gone"
    return classify_pod(pod, now=time.monotonic(), first_seen=self._first_seen,
                        last_seen=self._last_seen,
                        policy=self.policy)

  def _resolve_pod_name(self) -> str | None:
    ns = self.cluster.namespace
    api = self.cluster.custom_api
    try:
      claim = api.get_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION, namespace=ns,
          plural=constants.CLAIMS_PLURAL, name=self.claim_name,
          _request_timeout=_REQUEST_TIMEOUT)
    except client.ApiException as e:
      if e.status == 404:
        return None
      raise
    sb = (claim.get("status") or {}).get("sandbox") or {}
    sandbox_name = sb.get("name") or sb.get("Name")
    if not sandbox_name:
      return None
    # The pod normally shares the Sandbox's name; the controller annotates the
    # Sandbox when it does not (mirrors the SDK's Sandbox.get_pod_name).
    try:
      sandbox = api.get_namespaced_custom_object(
          group=constants.SANDBOX_GROUP, version=constants.SANDBOX_VERSION,
          namespace=ns, plural=constants.SANDBOXES_PLURAL, name=sandbox_name,
          _request_timeout=_REQUEST_TIMEOUT)
    except client.ApiException as e:
      if e.status != 404:
        raise
      return sandbox_name
    annotations = (sandbox.get("metadata") or {}).get("annotations") or {}
    return annotations.get(constants.POD_NAME_ANNOTATION) or sandbox_name

  def _read_pod(self, name: str):
    try:
      return self.cluster.core_api.read_namespaced_pod_status(
          name, self.cluster.namespace, _request_timeout=_REQUEST_TIMEOUT)
    except client.ApiException as e:
      if e.status == 404:
        return None
      raise
