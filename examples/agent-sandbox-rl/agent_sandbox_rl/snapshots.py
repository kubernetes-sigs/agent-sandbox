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

"""GKE Pod Snapshot primitives (opt-in).

The `k8s-agent-sandbox` SDK ships a GKE-only extension
(`k8s_agent_sandbox.gke_extensions.snapshots`) whose sandboxes can snapshot
(memory + filesystem), suspend, resume, and restore. This module is the thin
fleet-side glue: build that client bound to a fleet cluster's own context, map
the extension's result objects onto exceptions, and read a pod's ``PodRestored``
condition. `SandboxHandle` and `SandboxFleet` expose the user-facing methods.

Nothing here runs unless a cluster sets ``ClusterConfig(snapshots=True)``.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass

from .exceptions import SnapshotError, SnapshotsUnavailable

logger = logging.getLogger("agent_sandbox_rl.snapshots")

HINT = (
    "snapshot operations need the GKE Pod Snapshot-capable SDK client: set "
    "ClusterConfig(snapshots=True) for this cluster (requires a GKE cluster with "
    "Pod Snapshots enabled, a gVisor runtime class, and a PodSnapshotStorageConfig "
    "+ PodSnapshotPolicy grouped by agents.x-k8s.io/sandbox-name-hash — see "
    "README → Snapshots)."
)

_REQUIRED_METHODS = ("suspend", "resume", "restore", "is_suspended")


def capable(sandbox) -> bool:
  """Duck-typed: does this SDK sandbox carry the snapshot extension's surface?"""
  return sandbox is not None and all(
      callable(getattr(sandbox, m, None)) for m in _REQUIRED_METHODS)


def check(result, op: str):
  """Map the extension's result objects (``success`` flag) onto an exception.

  The extension does not raise for operational failures: it returns
  ``SuspendResponse`` / ``ResumeResponse`` / ``RestorationResponse`` /
  ``SnapshotResponse`` / ``ListSnapshotResult`` / ``DeleteSnapshotResult`` with
  ``success=False`` and an ``error_reason``. Callers here want exceptions — a
  failed suspend must never read as a parked sandbox.
  """
  if getattr(result, "success", False):
    return result
  reason = getattr(result, "error_reason", "") or "no error_reason reported"
  raise SnapshotError(f"{op} failed: {reason}")


def _snapshot_client_class():
  """The SDK's ``PodSnapshotSandboxClient``, subclassed to take the fleet's helper.

  The stock constructor verifies the PodSnapshot CRD through a **fresh**
  ``K8sHelper()`` (the ambient kube context) before a caller can inject one, so
  for a multi-context fleet it would check — and possibly reject — the wrong
  cluster. The subclass runs the base ``SandboxClient`` init, injects the
  per-cluster helper, then performs the same check against *that* cluster.
  """
  try:
    from k8s_agent_sandbox import SandboxClient
    from k8s_agent_sandbox.gke_extensions.snapshots import PodSnapshotSandboxClient
  except ImportError as e:
    raise SnapshotsUnavailable(
        "the installed k8s-agent-sandbox has no gke_extensions.snapshots "
        "(upgrade the SDK); " + HINT) from e

  class FleetPodSnapshotClient(PodSnapshotSandboxClient):
    def __init__(self, k8s_helper, **kwargs):
      SandboxClient.__init__(self, **kwargs)
      self.k8s_helper = k8s_helper
      self.snapshot_crd_installed = self._check_snapshot_crd_installed()
      if not self.snapshot_crd_installed:
        raise SnapshotsUnavailable(
            "the PodSnapshot CRD (podsnapshot.gke.io/v1) is not served on this "
            "cluster — enable Pod Snapshots on the GKE cluster; " + HINT)

  return FleetPodSnapshotClient


def build_snapshot_client(k8s_helper, *, tracer_config=None):
  """Build the snapshot-capable SDK client bound to ``k8s_helper``'s cluster."""
  cls = _snapshot_client_class()
  kwargs = {"tracer_config": tracer_config} if tracer_config is not None else {}
  return cls(k8s_helper, **kwargs)


@dataclass
class RestoreStatus:
  """What a pod's ``PodRestored`` condition says.

  ``restored`` is ``None`` when the condition could not be read (API error),
  ``False`` when the pod started fresh (no condition, or status not True), and
  ``True`` when GKE restored it from a snapshot. ``message`` is the condition
  message (GKE includes the snapshot uid) or the read error.
  """

  restored: bool | None
  message: str = ""

  def from_snapshot(self, snapshot_uid: str | None = None) -> bool:
    """True when restored — and, if ``snapshot_uid`` is given, from *that* one.

    The uid is matched as a whole token in the condition message (GKE names the
    snapshot there), so ``uid-1`` does not match a message about ``uid-10``.
    """
    if not self.restored:
      return False
    if not snapshot_uid:
      return True
    return re.search(rf"(?<![\w.-]){re.escape(snapshot_uid)}(?![\w.-])",
                     self.message) is not None


def pod_restore_status(core_api, pod_name: str, namespace: str) -> RestoreStatus:
  """Read the pod's ``PodRestored`` condition. Never raises."""
  try:
    pod = core_api.read_namespaced_pod(pod_name, namespace)
  except Exception as e:  # noqa: BLE001 — informational; callers record it
    return RestoreStatus(None, f"read failed: {e}")
  conditions = getattr(getattr(pod, "status", None), "conditions", None) or []
  for cond in conditions:
    if getattr(cond, "type", "") == "PodRestored":
      msg = getattr(cond, "message", None) or getattr(cond, "reason", None) or ""
      return RestoreStatus(str(getattr(cond, "status", "")) == "True", msg)
  return RestoreStatus(False, "no PodRestored condition (fresh start)")
