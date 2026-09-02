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

from agent_sandbox_rl.adapters.openhands import (
    BoundHandleClient,
    FleetWorkspaceClient,
    make_fleet_workspace,
    make_handle_workspace,
)
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
