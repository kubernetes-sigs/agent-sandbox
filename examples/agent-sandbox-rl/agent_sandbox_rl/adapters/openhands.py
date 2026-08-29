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

"""OpenHands adapter: run `AgentSandboxWorkspace` on fleet-acquired pods.

The standalone integration (`clients/integrations/openhands`) claims its own pod
per workspace and deletes the claim on close. This adapter inverts ownership the
same way `adapters.r2egym` does for R2E-Gym: the **fleet** acquires and releases
pods (pools, placement, budgets, accounting), and the workspace merely binds one.
``workspace close -> fleet.release(handle)`` — the workspace never deletes.

Usage::

    from agent_sandbox_rl import SandboxFleet, FleetConfig
    from agent_sandbox_rl.adapters.openhands import make_fleet_workspace

    fleet = SandboxFleet(FleetConfig(...))   # pools of agent-server pods
    fleet.load_tasks(...)
    fleet.setup()

    with make_fleet_workspace(fleet, task, api_key=POOL_KEY) as workspace:
        conversation = Conversation(agent=agent, workspace=workspace)
        ...

The mechanism is the workspace's ``sandbox_client`` injection seam: the shim's
``create_sandbox`` is a fleet acquire and the returned handle view's
``terminate`` is a fleet release, so the workspace class runs byte-identical.

Requires the `openhands-k8s-agent-sandbox` integration (which pulls
`openhands-sdk`). Importing THIS module is cheap; OpenHands imports happen
inside `make_fleet_workspace` so the core package works without them.
"""

from __future__ import annotations

import logging

logger = logging.getLogger("agent_sandbox_rl.adapters.openhands")

_HINT = (
    "requires the OpenHands integration — `pip install openhands-k8s-agent-sandbox` "
    "(or `pip install -e clients/integrations/openhands` from the repo root)."
)

# Workspace kwargs that would fight the fleet over pod lifecycle/identity.
_FLEET_OWNED_KWARGS = ("sandbox_client", "warmpool", "ttl_s")


class FleetWorkspaceClient:
  """Duck-typed ``sandbox_client`` backed by a fleet.

  ``create_sandbox()`` acquires a warm pod from the fleet (the ``warmpool``
  argument and lifecycle kwargs the workspace passes are ignored — the fleet
  owns pools, placement, and claim TTLs). The returned view's ``terminate()``
  releases the handle back to the fleet instead of deleting anything itself.
  """

  def __init__(self, fleet, task):
    self._fleet = fleet
    self._task = task
    self.handle = None

  def create_sandbox(self, warmpool, **_fleet_owned):  # noqa: ARG002
    handle = self._fleet.acquire(self._task)
    self.handle = handle
    fleet = self._fleet

    class _HandleView:
      claim_name = handle.claim_name
      sandbox_id = handle.sandbox_id

      @staticmethod
      def get_pod_ip():
        if handle.pod_ip:
          return handle.pod_ip
        sandbox = handle.sandbox
        if sandbox is not None:
          return sandbox.get_pod_ip()
        return None

      @staticmethod
      def terminate():
        # Ownership inversion: release to the fleet, never delete directly.
        fleet.release(handle)

    return _HandleView()


def make_fleet_workspace(fleet, task, *, namespace: str | None = None,
                         **workspace_kwargs):
  """Build an `AgentSandboxWorkspace` bound to a fleet-acquired pod.

  The fleet owns the pod's lifecycle: the workspace's ``close()`` releases the
  handle via ``fleet.release`` (through the shim), and lifecycle knobs belong
  to the fleet — passing ``warmpool``, ``ttl_s``, or ``sandbox_client`` here
  raises. ``namespace`` matters only for router mode (routing headers); pass
  the fleet cluster's namespace when using ``router_url``.

  All other kwargs (``api_key``, ``router_url``, ``router_auth_token``,
  ``server_port``, timeouts, ...) pass through to the workspace.
  """
  for key in _FLEET_OWNED_KWARGS:
    if key in workspace_kwargs:
      raise ValueError(
          f"{key!r} is fleet-owned when using make_fleet_workspace — "
          "configure it on the fleet, not the workspace")
  try:
    from openhands_k8s_agent_sandbox import AgentSandboxWorkspace
  except ImportError as e:
    raise RuntimeError(f"agent_sandbox_rl.adapters.openhands {_HINT}") from e

  kwargs = dict(workspace_kwargs)
  if namespace is not None:
    kwargs["namespace"] = namespace
  return AgentSandboxWorkspace(
      # Informational only — the claim goes through the fleet, which already
      # planned a pool for this task's image.
      warmpool=f"fleet:{task.image}",
      sandbox_client=FleetWorkspaceClient(fleet, task),
      **kwargs,
  )
