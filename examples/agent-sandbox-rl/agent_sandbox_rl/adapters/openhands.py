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
    from agent_sandbox_rl.adapters.openhands import (
        DEFAULT_AGENT_SERVER_IMAGE, agent_server_template, make_fleet_workspace)

    fleet = SandboxFleet(FleetConfig(                  # pools of agent-server pods
        template=agent_server_template(session_key=POOL_KEY,
                                       server_image=DEFAULT_AGENT_SERVER_IMAGE),
        ...))
    fleet.load_tasks(...)                               # plain task images
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
import shlex

logger = logging.getLogger("agent_sandbox_rl.adapters.openhands")

_HINT = (
    "requires the OpenHands integration (openhands-k8s-agent-sandbox) — "
    "`pip install -e clients/integrations/openhands` from the repo root."
)

# Workspace kwargs that would fight the fleet over pod lifecycle/identity.
_FLEET_OWNED_KWARGS = ("sandbox_client", "warmpool", "ttl_s")

# The agent-server image the standalone integration documents
# (clients/integrations/openhands/configs/agent-server-template.yaml).
DEFAULT_AGENT_SERVER_IMAGE = "ghcr.io/openhands/agent-server:1.44.1-python"
# Where an injected server is staged inside the pod (emptyDir shared by the
# init container that copies it and the runtime container that runs it).
AGENT_SERVER_MOUNT = "/oh"
# The published image (DEFAULT_AGENT_SERVER_IMAGE, checked with crane on
# 2026-10-22) ships one file, /usr/local/bin/openhands-agent-server (a
# self-contained PyInstaller binary); it has no /opt/oh. The other two paths
# are layouts of source-built images and are tried after it.
_SERVER_COPY_CMD = (
    f"cp -a /usr/local/bin/openhands-agent-server {AGENT_SERVER_MOUNT}/ 2>/dev/null"
    f" || cp -a /opt/oh/. {AGENT_SERVER_MOUNT}/ 2>/dev/null"
    f" || cp -a /openhands/poetry/*/bin/agent-server"
    f" {AGENT_SERVER_MOUNT}/openhands-agent-server 2>/dev/null"
    # Fail the init container, loudly, rather than let the runtime container
    # crash-loop later with the server missing.
    " || { echo 'no openhands-agent-server found in server_image' >&2; exit 1; }")


def agent_server_command(server_port: int = 8000,
                         workspace_dir: str | None = "/testbed") -> list[str]:
  """The pod command that serves the OpenHands agent-server from a task image.

  Prefers a server staged at `AGENT_SERVER_MOUNT` (see `agent_server_template`'s
  ``server_image``), then one baked into the image at
  ``/usr/local/bin/openhands-agent-server``. Before serving it puts a repo venv
  (``<workspace_dir>/.venv/bin``) first on ``PATH``, links ``workspace_dir`` and
  ``/workspace`` to each other when only one exists (the agent-server works in
  ``/workspace``; task images ship ``/testbed``), and marks every repo safe for
  git. ``workspace_dir=None`` skips the venv and the symlinks.
  """
  mount = AGENT_SERVER_MOUNT
  prep = f"chmod +x {mount}/openhands-agent-server 2>/dev/null || true; "
  if workspace_dir:
    wd = shlex.quote(workspace_dir.rstrip("/"))
    prep += (
        f'if [ -d {wd}/.venv/bin ]; then export PATH={wd}/.venv/bin:"${{PATH}}"; fi; '
        f"([ -d {wd} ] && [ ! -e /workspace ] && ln -s {wd} /workspace 2>/dev/null || true); "
        f"([ -d /workspace ] && [ ! -e {wd} ] && ln -s /workspace {wd} 2>/dev/null || true); ")
  prep += "git config --global --add safe.directory '*' 2>/dev/null || true; cd /; "
  serve = (
      f"if [ -x {mount}/openhands-agent-server ]; then"
      f" exec {mount}/openhands-agent-server --host 0.0.0.0 --port {server_port};"
      f" elif [ -x /usr/local/bin/openhands-agent-server ]; then"
      f" exec /usr/local/bin/openhands-agent-server --host 0.0.0.0 --port {server_port};"
      f" else echo 'openhands-agent-server not found' >&2; exit 1; fi")
  return ["sh", "-c", prep + serve]


def agent_server_template(*, server_image: str | None = None,
                          session_key: str | None = None,
                          session_key_secret: tuple[str, str] | None = None,
                          allow_unauthenticated: bool = False,
                          server_port: int = 8000,
                          workspace_dir: str | None = "/testbed",
                          readiness_timeout_s: int = 300,
                          resources=None, limits: dict | None = None,
                          node_selector: dict | None = None,
                          tolerations: list | None = None,
                          runtime_class: str | None = None,
                          image_pull_policy: str = "IfNotPresent",
                          extra_pod_spec: dict | None = None):
  """A `TemplateSpec` whose pods serve the OpenHands agent-server, ready to pass
  as ``FleetConfig(template=...)`` so `make_fleet_workspace` /
  `make_handle_workspace` can bind them.

  Two layouts, chosen by ``server_image``:

  - ``None``: every task image already contains the agent-server (for example
    `DEFAULT_AGENT_SERVER_IMAGE` itself, or a derived image). The pod runs it
    from ``/usr/local/bin``. A plain repo image (R2E-Gym, SWE-bench) has no
    server, so with ``None`` its pod exits with ``openhands-agent-server not
    found`` and crash-loops; pass ``server_image`` for those.
  - an image: the task image is a plain repo image (R2E-Gym, SWE-bench); an init
    container copies the server out of ``server_image`` into an emptyDir at
    `AGENT_SERVER_MOUNT` and the runtime container runs it from there. One
    agent-server image serves every task image without rebuilding them.

  The runtime container listens on ``server_port`` and probes ``/health`` twice:
  a startupProbe gives a pod ``readiness_timeout_s`` to come up (the kubelet
  restarts a container that has not served by then), and a 1 s readinessProbe
  with the default threshold gates traffic after that, so a Ready pod is a serving
  agent-server (the workspace's short health wait relies on it) and a server that
  hangs later drops out of Ready within seconds. The session key is exposed as
  ``OH_SESSION_API_KEYS_0``: ``session_key`` as a literal value, or
  ``session_key_secret=(secret_name, key)`` as a ``secretKeyRef``. Pass the same
  key as the workspace's ``api_key``. One of them is required: the server listens
  on ``0.0.0.0``, and an unauthenticated server is only for a private, trusted
  network, which ``allow_unauthenticated=True`` opts into explicitly.

  ``resources`` defaults to 500m / 1Gi requests with 2 CPU / 4Gi ``limits``. The
  pod never goes without limits (task images run untrusted code): a
  caller-supplied ``resources`` gets limits equal to its requests unless
  ``limits`` is given too (a request above a fixed limit would be an invalid pod,
  and limits equal to requests are always valid); the init container that stages
  the server carries the same requests and limits. ``tolerations`` and
  ``extra_pod_spec`` go through `TemplateSpec.extra_pod_spec`; ``extra_pod_spec``
  is merged last and wins, except that its ``containers``, ``initContainers``,
  ``volumes`` and ``tolerations`` are appended to the preset's rather than
  replacing them (the server volume must stay mounted). The pod mounts no
  ServiceAccount token and shares no PID namespace.
  """
  from ..config import ResourceSpec, TemplateSpec
  if session_key and session_key_secret:
    raise ValueError("pass session_key or session_key_secret, not both")
  if not session_key and not session_key_secret and not allow_unauthenticated:
    raise ValueError(
        "agent_server_template: pass session_key or session_key_secret; the agent-server "
        "listens on 0.0.0.0, so a server without a key is only for a private, trusted "
        "network (allow_unauthenticated=True opts in)")
  if resources is None:
    resources = ResourceSpec(cpu="500m", memory="1Gi")
    if limits is None:
      limits = {"cpu": "2", "memory": "4Gi"}
  elif limits is None:
    # Untrusted task code never runs without limits; limits equal to the
    # caller's requests are always valid and give the pod Guaranteed QoS.
    limits = {"cpu": resources.cpu, "memory": resources.memory}
  container_resources: dict = {}
  if limits:
    container_resources["limits"] = dict(limits)
  env = []
  if session_key:
    env.append({"name": "OH_SESSION_API_KEYS_0", "value": session_key})
  elif session_key_secret:
    name, key = session_key_secret
    env.append({"name": "OH_SESSION_API_KEYS_0",
                "valueFrom": {"secretKeyRef": {"name": name, "key": key}}})
  period = 2
  runtime = {
      "ports": [{"containerPort": server_port}],
      # A readinessProbe never gives up on start-up (its threshold only governs
      # Ready -> NotReady), so the start-up budget is a startupProbe.
      "startupProbe": {
          "httpGet": {"path": "/health", "port": server_port},
          "periodSeconds": period,
          "failureThreshold": max(1, readiness_timeout_s // period),
      },
      "readinessProbe": {
          "httpGet": {"path": "/health", "port": server_port},
          "periodSeconds": 1,
      },
  }
  if container_resources:
    runtime["resources"] = dict(container_resources)
  if env:
    runtime["env"] = env
  pod: dict = {
      "automountServiceAccountToken": False,
      "shareProcessNamespace": False,
      "containers": [runtime],
  }
  if server_image:
    runtime["volumeMounts"] = [{"name": "oh", "mountPath": AGENT_SERVER_MOUNT}]
    pod["initContainers"] = [{
        "name": "oh-server",
        "image": server_image,
        "imagePullPolicy": image_pull_policy,
        "command": ["sh", "-c", _SERVER_COPY_CMD],
        "volumeMounts": [{"name": "oh", "mountPath": AGENT_SERVER_MOUNT}],
        # Init requests do not add to the pod's (max of init and app containers),
        # and a namespace quota rejects a container without them.
        "resources": {"requests": {"cpu": resources.cpu, "memory": resources.memory},
                      **container_resources},
    }]
    pod["volumes"] = [{"name": "oh", "emptyDir": {}}]
  if tolerations:
    pod["tolerations"] = list(tolerations)
  if extra_pod_spec:
    extra = dict(extra_pod_spec)
    # List-valued keys the preset fills itself are appended to, not replaced:
    # the caller's (unnamed or runtime-named) container dict refines ours (the
    # fleet merges containers by name later), and replacing `volumes` or
    # `initContainers` would leave both containers mounting a volume that no
    # longer exists.
    for key in ("containers", "initContainers", "volumes", "tolerations"):
      if key in extra:
        pod[key] = list(pod.get(key, [])) + list(extra.pop(key))
    pod.update(extra)
  return TemplateSpec(
      resources=resources, image_pull_policy=image_pull_policy,
      keepalive_command=agent_server_command(server_port, workspace_dir),
      runtime_class=runtime_class, node_selector=node_selector, extra_pod_spec=pod)


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
