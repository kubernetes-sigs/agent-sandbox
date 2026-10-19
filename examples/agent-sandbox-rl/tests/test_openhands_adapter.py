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

"""Tests for the OpenHands fleet adapter.

The shim itself has no OpenHands dependency (lazy imports), so most tests run
with plain fakes; `make_fleet_workspace` is exercised against a stub
`openhands_k8s_agent_sandbox` module injected into sys.modules — same pattern
the r2egym adapter tests use for the optional R2E-Gym dependency.
"""

import sys
import types

import pytest

from unittest.mock import MagicMock

from agent_sandbox_rl.adapters.openhands import (
    AGENT_SERVER_MOUNT,
    DEFAULT_AGENT_SERVER_IMAGE,
    BoundHandleClient,
    FleetWorkspaceClient,
    agent_server_command,
    agent_server_template,
    make_fleet_workspace,
    make_handle_workspace,
)
from agent_sandbox_rl.config import ResourceSpec
from agent_sandbox_rl.resources import Resources
from agent_sandbox_rl.sources import Task


class FakeSdkSandbox:
  def __init__(self, pod_ip="10.8.8.8"):
    self._pod_ip = pod_ip

  def get_pod_ip(self):
    return self._pod_ip


class FakeCluster:
  def __init__(self, namespace="fleet-ns"):
    self.namespace = namespace


class FakeHandle:
  def __init__(self, pod_ip="10.1.2.3", sandbox=None, task=None, cluster=None):
    self.claim_name = "claim-42"
    self.sandbox_id = "sbx-42"
    self.pod_ip = pod_ip
    self.sandbox = sandbox
    self.task = task
    self._cluster = cluster


class FakeFleet:
  def __init__(self, handle=None):
    self.handle = handle or FakeHandle()
    self.acquired = []
    self.released = []

  def acquire(self, task):
    self.acquired.append(task)
    return self.handle

  def release(self, handle):
    self.released.append(handle)


def make_task():
  return Task(id="t1", image="img:1", metadata={})


# ------------------------------------------------------------------- shim


def test_create_sandbox_acquires_from_fleet():
  fleet = FakeFleet()
  task = make_task()
  client = FleetWorkspaceClient(fleet, task)
  view = client.create_sandbox("ignored-pool", sandbox_ready_timeout=5,
                               shutdown_after_seconds=999, labels={"x": "y"})
  assert fleet.acquired == [task]
  assert view.claim_name == "claim-42"
  assert view.sandbox_id == "sbx-42"
  assert view.get_pod_ip() == "10.1.2.3"


def test_pod_ip_falls_back_to_sdk_sandbox():
  fleet = FakeFleet(FakeHandle(pod_ip=None, sandbox=FakeSdkSandbox("10.8.8.8")))
  view = FleetWorkspaceClient(fleet, make_task()).create_sandbox("p")
  assert view.get_pod_ip() == "10.8.8.8"


def test_terminate_releases_to_fleet():
  fleet = FakeFleet()
  view = FleetWorkspaceClient(fleet, make_task()).create_sandbox("p")
  assert view.releases_on_terminate is True   # workspace logs a release
  view.terminate()
  assert fleet.released == [fleet.handle]


# ------------------------------------------------------- make_fleet_workspace


@pytest.fixture
def fake_openhands_integration(monkeypatch):
  """Stub openhands_k8s_agent_sandbox so no OpenHands install is needed."""
  captured = {}

  class FakeWorkspace:
    def __init__(self, **kwargs):
      captured.update(kwargs)

  module = types.ModuleType("openhands_k8s_agent_sandbox")
  module.AgentSandboxWorkspace = FakeWorkspace
  monkeypatch.setitem(sys.modules, "openhands_k8s_agent_sandbox", module)
  return captured


def test_make_fleet_workspace_wires_shim(fake_openhands_integration):
  fleet, task = FakeFleet(), make_task()
  make_fleet_workspace(fleet, task, namespace="rl", api_key="pool-key")
  kwargs = fake_openhands_integration
  assert kwargs["warmpool"] == "fleet:img:1"          # informational marker
  assert isinstance(kwargs["sandbox_client"], FleetWorkspaceClient)
  assert kwargs["namespace"] == "rl"
  assert kwargs["api_key"] == "pool-key"


@pytest.mark.parametrize("key,value", [
    ("ttl_s", 60), ("warmpool", "p"), ("sandbox_client", object()),
])
def test_fleet_owned_kwargs_rejected(fake_openhands_integration, key, value):
  with pytest.raises(ValueError, match="fleet-owned"):
    make_fleet_workspace(FakeFleet(), make_task(), **{key: value})


def test_missing_integration_raises_clear_hint(monkeypatch):
  monkeypatch.setitem(sys.modules, "openhands_k8s_agent_sandbox", None)
  with pytest.raises(RuntimeError, match="openhands-k8s-agent-sandbox"):
    make_fleet_workspace(FakeFleet(), make_task())


# ------------------------------------------------------ make_handle_workspace


def test_bound_handle_client_binds_without_acquiring():
  handle = FakeHandle()
  view = BoundHandleClient(handle).create_sandbox("ignored", labels={"x": "y"})
  assert view.claim_name == "claim-42"
  assert view.get_pod_ip() == "10.1.2.3"
  view.terminate()          # must be a no-op: fleet.run owns the release
  # No fleet in sight — nothing to have released; the handle is untouched.
  assert handle.pod_ip == "10.1.2.3"
  # ...and the view says so, so the workspace logs a detach, not a release.
  assert view.releases_on_terminate is False


def test_make_handle_workspace_wires_bound_client(fake_openhands_integration):
  handle = FakeHandle(task=make_task(), cluster=FakeCluster("rl-ns"))
  make_handle_workspace(handle, api_key="pool-key")
  kwargs = fake_openhands_integration
  assert kwargs["warmpool"] == "handle:img:1"       # informational marker
  assert isinstance(kwargs["sandbox_client"], BoundHandleClient)
  assert kwargs["sandbox_client"].handle is handle
  assert kwargs["api_key"] == "pool-key"


def test_handle_workspace_namespace_defaults_from_cluster(
    fake_openhands_integration):
  handle = FakeHandle(task=make_task(), cluster=FakeCluster("fleet-ns"))
  make_handle_workspace(handle)
  assert fake_openhands_integration["namespace"] == "fleet-ns"


def test_handle_workspace_namespace_override_wins(fake_openhands_integration):
  handle = FakeHandle(task=make_task(), cluster=FakeCluster("fleet-ns"))
  make_handle_workspace(handle, namespace="explicit")
  assert fake_openhands_integration["namespace"] == "explicit"


@pytest.mark.parametrize("key,value", [
    ("ttl_s", 60), ("warmpool", "p"), ("sandbox_client", object()),
])
def test_handle_workspace_fleet_owned_kwargs_rejected(
    fake_openhands_integration, key, value):
  with pytest.raises(ValueError, match="fleet-owned"):
    make_handle_workspace(FakeHandle(), **{key: value})


# ------------------------------------------------------- agent-server template


def _render(spec, image="repo/task:1"):
  return Resources(MagicMock(), MagicMock(), "ns")._template_manifest(
      image, "oh-img-1", spec)["spec"]["podTemplate"]["spec"]


def test_agent_server_template_defaults_serve_from_the_task_image():
  spec = agent_server_template()
  pod = _render(spec)
  runtime = pod["containers"][0]
  assert runtime["name"] == "agent-runtime" and runtime["image"] == "repo/task:1"
  assert runtime["command"] == agent_server_command(8000)
  assert "--port 8000" in runtime["command"][2] and "/usr/local/bin/openhands-agent-server" in runtime["command"][2]
  assert runtime["ports"] == [{"containerPort": 8000}]
  assert runtime["startupProbe"]["httpGet"] == {"path": "/health", "port": 8000}
  assert runtime["startupProbe"]["failureThreshold"] * runtime["startupProbe"]["periodSeconds"] == 300
  assert runtime["readinessProbe"]["httpGet"] == {"path": "/health", "port": 8000}
  assert runtime["readinessProbe"]["periodSeconds"] == 1          # default threshold:
  assert "failureThreshold" not in runtime["readinessProbe"]       # a hung server drops out fast
  assert runtime["resources"] == {"requests": {"cpu": "500m", "memory": "1Gi"},
                                  "limits": {"cpu": "2", "memory": "4Gi"}}
  assert "env" not in runtime and "initContainers" not in pod and "volumes" not in pod
  assert pod["automountServiceAccountToken"] is False and pod["shareProcessNamespace"] is False
  assert spec.image_pull_policy == "IfNotPresent" and runtime["imagePullPolicy"] == "IfNotPresent"


def test_agent_server_template_injects_the_server_from_another_image():
  spec = agent_server_template(server_image=DEFAULT_AGENT_SERVER_IMAGE, session_key="k1",
                               server_port=9000, tolerations=[{"key": "workload", "value": "sandbox"}],
                               node_selector={"pool": "sbx"}, runtime_class="gvisor",
                               image_pull_policy="Always", readiness_timeout_s=60)
  pod = _render(spec)
  init = pod["initContainers"][0]
  assert init["name"] == "oh-server" and init["image"] == DEFAULT_AGENT_SERVER_IMAGE
  assert init["imagePullPolicy"] == "Always" and init["volumeMounts"] == [{"name": "oh", "mountPath": AGENT_SERVER_MOUNT}]
  assert "cp -a" in init["command"][2] and AGENT_SERVER_MOUNT in init["command"][2]
  assert pod["volumes"] == [{"name": "oh", "emptyDir": {}}]
  runtime = pod["containers"][0]
  assert runtime["volumeMounts"] == [{"name": "oh", "mountPath": AGENT_SERVER_MOUNT}]
  assert f"{AGENT_SERVER_MOUNT}/openhands-agent-server --host 0.0.0.0 --port 9000" in runtime["command"][2]
  assert runtime["ports"] == [{"containerPort": 9000}]
  assert runtime["readinessProbe"]["httpGet"]["port"] == 9000
  assert runtime["startupProbe"]["failureThreshold"] == 30
  assert init["resources"] == {"requests": {"cpu": "500m", "memory": "1Gi"},
                               "limits": {"cpu": "2", "memory": "4Gi"}}
  assert runtime["env"] == [{"name": "OH_SESSION_API_KEYS_0", "value": "k1"}]
  assert pod["tolerations"] == [{"key": "workload", "value": "sandbox"}]
  assert pod["nodeSelector"] == {"pool": "sbx"} and pod["runtimeClassName"] == "gvisor"


def test_agent_server_template_session_key_from_a_secret_and_overrides():
  spec = agent_server_template(session_key_secret=("oh-key", "key"),
                               resources=ResourceSpec(cpu="1", memory="2Gi"),
                               workspace_dir=None,
                               extra_pod_spec={"serviceAccountName": "sa",
                                               "containers": [{"env": [{"name": "X", "value": "1"}]}]})
  pod = _render(spec)
  runtime = pod["containers"][0]
  assert {"name": "OH_SESSION_API_KEYS_0",
          "valueFrom": {"secretKeyRef": {"name": "oh-key", "key": "key"}}} in runtime["env"]
  assert {"name": "X", "value": "1"} in runtime["env"]       # caller's env merged in
  assert runtime["resources"] == {"requests": {"cpu": "1", "memory": "2Gi"}}   # no fixed limit
  assert pod["serviceAccountName"] == "sa" and len(pod["containers"]) == 1
  assert ".venv" not in runtime["command"][2] and "ln -s" not in runtime["command"][2]
  with pytest.raises(ValueError, match="not both"):
    agent_server_template(session_key="a", session_key_secret=("s", "k"))


def test_agent_server_template_appends_caller_lists_and_keeps_the_server_volume():
  spec = agent_server_template(
      server_image=DEFAULT_AGENT_SERVER_IMAGE, tolerations=[{"key": "a"}],
      extra_pod_spec={"volumes": [{"name": "dshm", "emptyDir": {"medium": "Memory"}}],
                      "initContainers": [{"name": "seed", "image": "busybox", "command": ["true"]}],
                      "tolerations": [{"key": "b"}]})
  pod = _render(spec)
  assert [v["name"] for v in pod["volumes"]] == ["oh", "dshm"]
  assert [c["name"] for c in pod["initContainers"]] == ["oh-server", "seed"]
  assert [t["key"] for t in pod["tolerations"]] == ["a", "b"]


def test_agent_server_template_limits_follow_the_caller():
  big = _render(agent_server_template(resources=ResourceSpec(cpu="4", memory="8Gi")))
  assert big["containers"][0]["resources"] == {"requests": {"cpu": "4", "memory": "8Gi"}}
  explicit = _render(agent_server_template(resources=ResourceSpec(cpu="4", memory="8Gi"),
                                           limits={"cpu": "8", "memory": "16Gi"},
                                           server_image="srv:1"))
  assert explicit["containers"][0]["resources"]["limits"] == {"cpu": "8", "memory": "16Gi"}
  assert explicit["initContainers"][0]["resources"]["limits"] == {"cpu": "8", "memory": "16Gi"}


def test_agent_server_command_quotes_the_workspace_dir_and_the_copy_fails_loudly():
  body = agent_server_command(8000, workspace_dir="/my repo")[2]
  assert "'/my repo'/.venv/bin" in body and "ln -s '/my repo' /workspace" in body
  init = _render(agent_server_template(server_image="x:1"))["initContainers"][0]
  assert "|| true" not in init["command"][2] and "exit 1" in init["command"][2]


def test_agent_server_command_prepares_the_workspace():
  cmd = agent_server_command(8000, workspace_dir="/testbed/")
  assert cmd[:2] == ["sh", "-c"]
  body = cmd[2]
  assert "/testbed/.venv/bin" in body and "ln -s /testbed /workspace" in body
  assert "safe.directory" in body and body.rstrip().endswith("fi")
