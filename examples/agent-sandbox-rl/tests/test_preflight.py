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


# ------------------------------------------------------ GKE Pod Snapshots (opt-in)


def _snapshot_cluster(policies=1):
  c = _healthy_cluster()
  c.custom_api = MagicMock()
  c.custom_api.list_namespaced_custom_object.return_value = {
      "items": [{"metadata": {"name": f"p{i}"}} for i in range(policies)]}
  return c


def _crd_router(missing=()):
  def route(name):
    if any(name.startswith(m) for m in missing):
      raise client.ApiException(status=404)
    return _crd(("v1",)) if name.endswith(constants.PODSNAPSHOT_GROUP) else _crd()
  return route


def test_snapshots_not_requested_adds_no_checks(monkeypatch):
  _patch(monkeypatch)
  rep = pf.preflight_cluster(_snapshot_cluster())
  assert not [c for c in rep.checks if c.name.startswith(("crd:podsnapshot", "snapshots:"))]


def test_snapshots_all_ok_with_gvisor(monkeypatch):
  _patch(monkeypatch, crd_for=_crd_router())
  c = _snapshot_cluster()
  rep = pf.preflight_cluster(c, snapshots=True, require_runtime_class="gvisor")
  names = {ch.name: ch for ch in rep.checks}
  assert rep.ok and not rep.warnings
  assert names["crd:podsnapshots"].ok and names["crd:podsnapshotmanualtriggers"].ok
  assert names["snapshots:runtime_class"].ok and names["snapshots:policy"].ok
  c.custom_api.list_namespaced_custom_object.assert_called_once_with(
      constants.PODSNAPSHOT_GROUP, constants.PODSNAPSHOT_VERSION, "ns",
      constants.PODSNAPSHOT_POLICIES_PLURAL)


def test_snapshots_missing_crd_is_hard_failure(monkeypatch):
  _patch(monkeypatch, crd_for=_crd_router(missing=("podsnapshots.",)))
  rep = pf.preflight_cluster(_snapshot_cluster(), snapshots=True,
                             require_runtime_class="gvisor")
  assert not rep.ok
  bad = {ch.name: ch.detail for ch in rep.failures}
  assert "crd:podsnapshots" in bad and "enable Pod Snapshots" in bad["crd:podsnapshots"]


def test_snapshots_require_runtime_class(monkeypatch):
  _patch(monkeypatch, crd_for=_crd_router())
  rep = pf.preflight_cluster(_snapshot_cluster(), snapshots=True)
  assert [ch.name for ch in rep.failures] == ["snapshots:runtime_class"]
  # A non-"gvisor" name may still be gVisor → warning, not failure.
  rep = pf.preflight_cluster(_snapshot_cluster(), snapshots=True,
                             require_runtime_class="gvisor-custom")
  assert rep.ok and [w.name for w in rep.warnings] == ["snapshots:runtime_class"]


def test_snapshots_missing_policy_is_warning(monkeypatch):
  _patch(monkeypatch, crd_for=_crd_router())
  rep = pf.preflight_cluster(_snapshot_cluster(policies=0), snapshots=True,
                             require_runtime_class="gvisor")
  assert rep.ok
  w = {ch.name: ch for ch in rep.warnings}
  assert "snapshots:policy" in w
  assert constants.SANDBOX_NAME_HASH_LABEL in w["snapshots:policy"].detail


def test_registry_preflight_passes_cluster_snapshot_flag(monkeypatch):
  seen = {}

  def fake(cluster, **kw):
    seen[cluster.name] = kw.get("snapshots")
    r = pf.PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr(pf, "preflight_cluster", fake)
  a = types.SimpleNamespace(name="a", config=types.SimpleNamespace(snapshots=True))
  b = types.SimpleNamespace(name="b", config=types.SimpleNamespace())
  pf.preflight([a, b])
  assert seen == {"a": True, "b": False}


def test_snapshots_served_version_mismatch_is_hard_failure(monkeypatch):
  def route(name):
    if name.endswith(constants.PODSNAPSHOT_GROUP):
      return _crd(("v1alpha1",))            # CRD present, but not serving v1
    return _crd()
  _patch(monkeypatch, crd_for=route)
  rep = pf.preflight_cluster(_snapshot_cluster(), snapshots=True,
                             require_runtime_class="gvisor")
  assert not rep.ok
  bad = {ch.name: ch.detail for ch in rep.failures}
  assert set(bad) == {"crd:podsnapshots", "crd:podsnapshotmanualtriggers"}
  assert all(detail == "v1alpha1" for detail in bad.values())
