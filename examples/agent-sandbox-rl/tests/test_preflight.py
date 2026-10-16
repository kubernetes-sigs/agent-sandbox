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


# --- Unmanaged templates: a NetworkPolicy must select the fleet's pods -------
FLEET_LABELS = {"app": "agent-sandbox-rl", "agent-sandbox-rl/run-id": "r1"}


def _policy(name, match_labels=None, match_expressions=None):
  exprs = [client.V1LabelSelectorRequirement(key=k, operator=op, values=v)
           for k, op, v in (match_expressions or [])]
  return client.V1NetworkPolicy(
      metadata=client.V1ObjectMeta(name=name),
      spec=client.V1NetworkPolicySpec(pod_selector=client.V1LabelSelector(
          match_labels=match_labels, match_expressions=exprs or None)))


def _patch_policies(monkeypatch, policies=None, error=None):
  api = MagicMock()
  if error is not None:
    api.list_namespaced_network_policy.side_effect = error
  else:
    api.list_namespaced_network_policy.return_value = types.SimpleNamespace(
        items=list(policies or []))
  monkeypatch.setattr(pf, "_networking_api", lambda c: api)
  return api


def test_networkpolicy_check_skipped_when_not_unmanaged(monkeypatch):
  _patch(monkeypatch)
  api = _patch_policies(monkeypatch)
  rep = pf.preflight_cluster(_healthy_cluster())
  assert not any(ch.name == "networkpolicy" for ch in rep.checks)
  api.list_namespaced_network_policy.assert_not_called()


def test_networkpolicy_check_ok_when_a_policy_selects_the_fleet(monkeypatch):
  _patch(monkeypatch)
  api = _patch_policies(monkeypatch, [
      _policy("other-team", {"app": "web"}),
      _policy("agent-sandbox-rl-fleet", {"app": "agent-sandbox-rl"})])
  rep = pf.preflight_cluster(_healthy_cluster(), unmanaged_pod_labels=FLEET_LABELS)
  check = next(ch for ch in rep.checks if ch.name == "networkpolicy")
  assert check.ok and check.detail == "agent-sandbox-rl-fleet"
  api.list_namespaced_network_policy.assert_called_once_with("ns")


def test_networkpolicy_check_warns_when_nothing_selects_the_fleet(monkeypatch):
  _patch(monkeypatch)
  _patch_policies(monkeypatch, [_policy("other-team", {"app": "web"})])
  rep = pf.preflight_cluster(_healthy_cluster(), unmanaged_pod_labels=FLEET_LABELS)
  assert rep.ok                                   # warning, not a failure
  warning = next(w for w in rep.warnings if w.name == "networkpolicy")
  assert "not isolated" in warning.detail and "app=agent-sandbox-rl" in warning.detail


def test_networkpolicy_check_list_forbidden_is_warning(monkeypatch):
  _patch(monkeypatch)
  _patch_policies(monkeypatch, error=client.ApiException(status=403))
  rep = pf.preflight_cluster(_healthy_cluster(), unmanaged_pod_labels=FLEET_LABELS)
  assert rep.ok
  warning = next(w for w in rep.warnings if w.name == "networkpolicy")
  assert "HTTP 403" in warning.detail


def test_pod_selector_covers_only_what_known_labels_confirm():
  known = {"app": "agent-sandbox-rl", "tier": "sbx"}
  sel = lambda **kw: _policy("p", **kw).spec.pod_selector  # noqa: E731
  assert pf.pod_selector_covers(sel(), known)                          # {} selects all
  assert pf.pod_selector_covers(sel(match_labels={"app": "agent-sandbox-rl"}), known)
  assert not pf.pod_selector_covers(sel(match_labels={"app": "web"}), known)
  assert not pf.pod_selector_covers(sel(match_labels={"missing": "x"}), known)
  assert pf.pod_selector_covers(sel(match_expressions=[("app", "In", ["agent-sandbox-rl"])]), known)
  assert not pf.pod_selector_covers(sel(match_expressions=[("app", "In", ["web"])]), known)
  assert pf.pod_selector_covers(sel(match_expressions=[("app", "NotIn", ["web"])]), known)
  assert not pf.pod_selector_covers(sel(match_expressions=[("app", "NotIn", ["agent-sandbox-rl"])]), known)
  assert pf.pod_selector_covers(sel(match_expressions=[("tier", "Exists", None)]), known)
  assert not pf.pod_selector_covers(sel(match_expressions=[("tier", "DoesNotExist", None)]), known)
  # Keys outside the known set may still be on the pods (the per-template
  # `sandbox` label, controller labels): no requirement on them confirms coverage.
  for op, values in (("DoesNotExist", None), ("NotIn", ["x"]), ("Exists", None), ("In", ["x"])):
    assert not pf.pod_selector_covers(sel(match_expressions=[("sandbox", op, values)]), known), op


# ------------------------------------------------------ GKE Pod Snapshots (opt-in)


def _policy_obj(name, labels):
  return {"metadata": {"name": name},
          "spec": {"snapshotGroupingRules": {"groupByLabelValue": {"labels": list(labels)}}}}


def _snapshot_cluster(policies=1, grouping=(constants.SANDBOX_NAME_HASH_LABEL,)):
  c = _healthy_cluster()
  c.custom_api = MagicMock()
  c.custom_api.list_namespaced_custom_object.return_value = {
      "items": [_policy_obj(f"p{i}", grouping) for i in range(policies)]}
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


def test_snapshots_policy_without_the_grouping_label_is_named_in_a_warning(monkeypatch):
  _patch(monkeypatch, crd_for=_crd_router())
  c = _snapshot_cluster(policies=2, grouping=("golden-pool",))
  rep = pf.preflight_cluster(c, snapshots=True, require_runtime_class="gvisor")
  assert rep.ok
  w = {ch.name: ch for ch in rep.warnings}["snapshots:policy"]
  assert "p0, p1" in w.detail and constants.SANDBOX_NAME_HASH_LABEL in w.detail
  assert "groupByLabelValue" in w.detail
  # one correctly grouped policy among others is enough, and it is named
  c.custom_api.list_namespaced_custom_object.return_value = {"items": [
      _policy_obj("other", ["golden-pool"]),
      _policy_obj("fleet", ["golden-pool", constants.SANDBOX_NAME_HASH_LABEL]),
      {"metadata": {"name": "broken"}, "spec": {"snapshotGroupingRules": "oops"}}]}
  rep = pf.preflight_cluster(c, snapshots=True, require_runtime_class="gvisor")
  ok = {ch.name: ch for ch in rep.checks}["snapshots:policy"]
  assert ok.ok and "1/3" in ok.detail and "fleet" in ok.detail


def test_policy_groups_by_tolerates_malformed_specs():
  assert pf.policy_groups_by(_policy_obj("p", ["a", constants.SANDBOX_NAME_HASH_LABEL]),
                             constants.SANDBOX_NAME_HASH_LABEL)
  assert not pf.policy_groups_by({"metadata": {"name": "p"}}, "x")
  assert not pf.policy_groups_by({"spec": {"snapshotGroupingRules": {"groupByLabelValue": {"labels": "x"}}}}, "x")
  assert not pf.policy_groups_by(None, "x")


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
