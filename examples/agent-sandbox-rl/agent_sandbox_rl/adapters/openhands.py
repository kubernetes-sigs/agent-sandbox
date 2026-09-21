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


def _handle_view(handle, on_terminate, *, releases=True):
  """Duck-typed SDK-sandbox view over a fleet handle for the workspace.

  ``releases`` tells the workspace what ``terminate()`` means here: a fleet
  release (True) or nothing at all (False, handle-bound mode) — it logs a
  release or a detach accordingly instead of claiming a release it never did.
  """

  class _HandleView:
    claim_name = handle.claim_name
    sandbox_id = handle.sandbox_id
    releases_on_terminate = releases

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
      on_terminate()

  return _HandleView()


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
    # Ownership inversion: release to the fleet, never delete directly.
    return _handle_view(handle, lambda: self._fleet.release(handle))


class BoundHandleClient:
  """Duck-typed ``sandbox_client`` over an ALREADY-acquired handle.

  For ``fleet.run(process_fn)`` flows: the fleet acquired the handle before
  calling ``process_fn(task, handle)`` and releases it after the function
  returns, so the workspace must neither acquire nor release — ``terminate()``
  is a no-op on the pod (closing the workspace only drops HTTP state), and the
  view says so (``releases_on_terminate=False``) so the workspace's cleanup
  logs a detach, not a release the fleet actually performs later.
  """

  def __init__(self, handle):
    self.handle = handle

  def create_sandbox(self, warmpool, **_fleet_owned):  # noqa: ARG002
    return _handle_view(self.handle, lambda: None, releases=False)


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


def make_handle_workspace(handle, *, namespace: str | None = None,
                          **workspace_kwargs):
  """Build an `AgentSandboxWorkspace` bound to an ALREADY-acquired handle.

  This is the form to use inside ``fleet.run(process_fn)``: the fleet passes
  ``(task, handle)`` to your function and releases the handle when it returns,
  so the workspace binds the pod without acquiring or releasing anything —
  ``workspace.cleanup()`` is safe to call and touches only HTTP state.

  ::

      def rollout(task, handle):
          workspace = make_handle_workspace(handle, api_key=POOL_KEY)
          try:
              conversation = Conversation(agent=agent, workspace=workspace)
              ...
          finally:
              workspace.cleanup()   # no-op on the pod; the fleet releases

      results = fleet.run(rollout)

  ``namespace`` defaults from the handle's cluster (it matters for router-mode
  headers); fleet-owned kwargs (``warmpool``, ``ttl_s``, ``sandbox_client``)
  raise, as in `make_fleet_workspace`.
  """
  for key in _FLEET_OWNED_KWARGS:
    if key in workspace_kwargs:
      raise ValueError(
          f"{key!r} is fleet-owned when using make_handle_workspace — "
          "configure it on the fleet, not the workspace")
  try:
    from openhands_k8s_agent_sandbox import AgentSandboxWorkspace
  except ImportError as e:
    raise RuntimeError(f"agent_sandbox_rl.adapters.openhands {_HINT}") from e

  kwargs = dict(workspace_kwargs)
  if namespace is None:
    namespace = getattr(getattr(handle, "_cluster", None), "namespace", None)
  if namespace is not None:
    kwargs["namespace"] = namespace
  task_image = getattr(getattr(handle, "task", None), "image", "?")
  return AgentSandboxWorkspace(
      warmpool=f"handle:{task_image}",     # informational; nothing is claimed
      sandbox_client=BoundHandleClient(handle),
      **kwargs,
  )
