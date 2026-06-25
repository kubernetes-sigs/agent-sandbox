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

  def exec(self, command) -> str:
    """Run a command inside the sandbox (router-free, via the pod's exec API).

    Uses a **thread-local** ``CoreV1Api`` (``Cluster.exec_core_api``): the
    kubernetes ``stream()`` (websocket) exec is not thread-safe across a shared
    client, so parallel execs stay isolated per thread — but the client is cached
    per thread rather than rebuilt per call, avoiding a leaked connection/thread
    pool on every exec.
    """
    core = self._cluster.exec_core_api()
    return exec_in_pod(core, self.pod_name, self._cluster.namespace, command)

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
    if self.sandbox is not None:
      self.sandbox.terminate()
    else:
      self._cluster.sandbox_client.delete_sandbox(
          self.claim_name, namespace=self._cluster.namespace)
