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

from unittest.mock import MagicMock, patch

import pytest

from agent_sandbox_rl import Cluster, ClusterConfig, ClusterRegistry, TemplateSpec
from agent_sandbox_rl import cluster as cluster_mod


def _cluster(**kw):
  cfg = ClusterConfig(**kw)
  return Cluster(cfg, api_client=MagicMock())


def test_cluster_builds_apis_and_resources():
  c = _cluster(name="c1", namespace="ns1")
  assert c.name == "c1" and c.namespace == "ns1"
  assert c.custom_api is not None and c.core_api is not None and c.apps_api is not None
  assert c.resources.namespace == "ns1"
  assert c.active_replicas == 0 and c.active_claims == 0


def test_build_api_client_used_when_not_injected():
  with patch.object(cluster_mod, "build_api_client", return_value=MagicMock()) as bac:
    Cluster(ClusterConfig(name="c"))
    bac.assert_called_once()


def test_sdk_clients_are_lazy():
  # Constructing a Cluster must NOT build the SDK K8sHelper/SandboxClient.
  with patch("k8s_agent_sandbox.k8s_helper.K8sHelper") as H:
    c = _cluster(name="c")
    H.assert_not_called()
    helper = c.k8s_helper          # now it builds + injects
    H.assert_called_once()
    assert helper.custom_objects_api is c.custom_api
    assert helper.core_v1_api is c.core_api
    assert c.k8s_helper is helper  # cached


def test_template_spec_merges_cluster_overrides():
  c = _cluster(name="c", runtime_class="gvisor",
               node_selector={"pool": "e2"}, image_pull_secret="ps")
  base = TemplateSpec()
  merged = c.template_spec(base)
  assert merged.runtime_class == "gvisor"
  assert merged.node_selector == {"pool": "e2"}
  assert merged.image_pull_secret == "ps"
  # base untouched
  assert base.runtime_class is None


def test_template_spec_preserves_colocate_replicas():
  # a pod-spec flag with no per-cluster override must survive the merge round-trip
  c = _cluster(name="c")
  merged = c.template_spec(TemplateSpec(colocate_replicas=True))
  assert merged.colocate_replicas is True


def test_capacity_gate():
  c = _cluster(name="c", max_replicas=5)
  c.active_replicas = 3
  assert c.has_capacity(2) is True
  assert c.has_capacity(3) is False
  unbounded = _cluster(name="d")
  assert unbounded.has_capacity(10_000) is True


def test_registry_add_get_iter():
  reg = ClusterRegistry([_cluster(name="a"), _cluster(name="b")])
  assert len(reg) == 2
  assert reg.names() == ["a", "b"]
  assert reg.get("a").name == "a"
  assert [c.name for c in reg] == ["a", "b"]


def test_registry_rejects_duplicates():
  reg = ClusterRegistry([_cluster(name="a")])
  with pytest.raises(ValueError):
    reg.add(_cluster(name="a"))


def test_registry_from_configs():
  with patch.object(cluster_mod, "build_api_client", return_value=MagicMock()):
    reg = ClusterRegistry.from_configs(
        [ClusterConfig(name="x"), ClusterConfig(name="y")])
    assert reg.names() == ["x", "y"]
