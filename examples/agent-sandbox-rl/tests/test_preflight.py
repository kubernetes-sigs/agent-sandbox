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

import types
from unittest.mock import MagicMock

from kubernetes import client

import agent_sandbox_rl.preflight as pf
from agent_sandbox_rl import constants


def _cluster():
  return types.SimpleNamespace(name="c", namespace="ns", api_client=MagicMock(),
                               apps_api=MagicMock(), core_api=MagicMock())


def _crd(served=("v1beta1",)):
  return types.SimpleNamespace(spec=types.SimpleNamespace(
      versions=[types.SimpleNamespace(name=v, served=True) for v in served]))


def _patch(monkeypatch, *, version_ok=True, crd_for=None, node_ok=True):
  ver = MagicMock()
  if version_ok:
    ver.get_code.return_value = types.SimpleNamespace(git_version="v1.30")
  else:
    ver.get_code.side_effect = Exception("connection refused")
  monkeypatch.setattr(pf, "_version_api", lambda c: ver)

  crd_api = MagicMock()
  crd_api.read_custom_resource_definition.side_effect = crd_for or (lambda name: _crd())
  monkeypatch.setattr(pf, "_crd_api", lambda c: crd_api)

  node = MagicMock()
  if not node_ok:
    node.read_runtime_class.side_effect = client.ApiException(status=404)
  monkeypatch.setattr(pf, "_node_api", lambda c: node)


def _healthy_cluster():
  c = _cluster()
  c.apps_api.read_namespaced_deployment.return_value = types.SimpleNamespace(
      status=types.SimpleNamespace(ready_replicas=1))
  c.core_api.read_namespace.return_value = object()
  return c


def test_all_ok(monkeypatch):
  _patch(monkeypatch)
  rep = pf.preflight_cluster(_healthy_cluster())
  assert rep.ok and not rep.failures


def test_unreachable_short_circuits(monkeypatch):
  _patch(monkeypatch, version_ok=False)
  rep = pf.preflight_cluster(_cluster())
  assert not rep.ok
  assert rep.failures[0].name == "reachable"
  assert len(rep.checks) == 1


def test_crd_missing(monkeypatch):
  def crd_for(name):
    if name.startswith(constants.WARMPOOLS_PLURAL):
      raise client.ApiException(status=404)
    return _crd()
  _patch(monkeypatch, crd_for=crd_for)
  rep = pf.preflight_cluster(_healthy_cluster())
  assert not rep.ok
  assert any(constants.WARMPOOLS_PLURAL in f.name for f in rep.failures)


def test_crd_wrong_version(monkeypatch):
  _patch(monkeypatch, crd_for=lambda name: _crd(served=("v1alpha1",)))
  rep = pf.preflight_cluster(_healthy_cluster())
  assert not rep.ok


def test_runtimeclass_required_missing(monkeypatch):
  _patch(monkeypatch, node_ok=False)
  rep = pf.preflight_cluster(_healthy_cluster(), require_runtime_class="gvisor")
  assert not rep.ok
  assert any("runtimeclass" in f.name for f in rep.failures)


def test_secret_required_missing(monkeypatch):
  _patch(monkeypatch)
  c = _healthy_cluster()
  c.core_api.read_namespaced_secret.side_effect = client.ApiException(status=404)
  rep = pf.preflight_cluster(c, image_pull_secret="dockerhub-pro")
  assert not rep.ok
  assert any("secret" in f.name for f in rep.failures)


def test_controller_down_is_warning_only(monkeypatch):
  _patch(monkeypatch)
  c = _healthy_cluster()
  c.apps_api.read_namespaced_deployment.side_effect = client.ApiException(status=404)
  rep = pf.preflight_cluster(c)
  assert rep.ok                                   # controller is warn-only
  assert any(w.name == "controller" for w in rep.warnings)
