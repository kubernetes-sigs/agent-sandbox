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

import pytest

from agent_sandbox_rl import ClusterConfig, FleetConfig, TemplateSpec, constants


def test_defaults_single_cluster_simple():
  cfg = FleetConfig()
  assert cfg.clusters == []
  assert cfg.max_concurrent == 1
  assert cfg.max_warmpool_size == 8
  assert cfg.window_size is None
  assert cfg.placement == "image-affinity"
  assert cfg.labels == constants.DEFAULT_LABELS
  assert cfg.template.keepalive_command == constants.KEEPALIVE_COMMAND
  assert cfg.template.resources.cpu == "250m"


def test_template_name_is_stable_and_prefixed():
  cfg = FleetConfig()
  img = "slimshetty/swebench-verified:sweb.eval.x86_64.astropy__astropy-12907"
  name = cfg.template_name(img)
  assert name.startswith("r2e-img-")
  assert name == cfg.template_name(img)            # stable
  assert len(name) == len("r2e-img-") + 12         # md5[:12]
  assert cfg.template_name("other") != name        # distinct per image


def test_cluster_config_fields():
  c = ClusterConfig(name="rl-testing-tomer", context="ctx", namespace="ns",
                    weight=2.0, max_replicas=50)
  assert c.name == "rl-testing-tomer"
  assert c.in_cluster is False
  assert c.weight == 2.0


def test_invalid_values_rejected():
  with pytest.raises(ValueError):
    FleetConfig(max_concurrent=0)
  with pytest.raises(ValueError):
    FleetConfig(max_warmpool_size=0)
  with pytest.raises(ValueError):
    FleetConfig(window_size=0)
  with pytest.raises(ValueError):
    FleetConfig(placement="bogus")
  with pytest.raises(ValueError):
    ClusterConfig(weight=0)
  with pytest.raises(ValueError):
    ClusterConfig(name="")


def test_template_spec_overrides():
  t = TemplateSpec(runtime_class="gvisor",
                   node_selector={"cloud.google.com/gke-nodepool": "e2-pool"},
                   image_pull_secret="dockerhub-pro")
  cfg = FleetConfig(template=t, max_concurrent=8)
  assert cfg.template.runtime_class == "gvisor"
  assert cfg.template.node_selector["cloud.google.com/gke-nodepool"] == "e2-pool"


def test_template_name_prefix_rejects_invalid():
  with pytest.raises(ValueError):          # pydantic ValidationError subclasses ValueError
    FleetConfig(template_name_prefix="Bad_Prefix!")
  # valid prefix is accepted
  assert FleetConfig(template_name_prefix="r2e-img-").template_name_prefix == "r2e-img-"


@pytest.mark.parametrize("bad", [
    "r2e..img-",      # consecutive dots -> empty DNS segment
    "r2e-.img-",      # segment ends with '-' before a dot
    "-r2e-img-",      # cannot start with '-'
    ".r2e-img-",      # cannot start with '.'
    "x" * 250,        # <prefix><12 hash> exceeds the 253 char DNS-1123 cap
])
def test_template_name_prefix_rejects_invalid_generated_name(bad):
  # The validator must reject prefixes whose *generated* name (<prefix><md5[:12]>)
  # isn't a valid DNS-1123 subdomain, not just non-[a-z0-9.-] prefixes.
  with pytest.raises(ValueError):
    FleetConfig(template_name_prefix=bad)


@pytest.mark.parametrize("good", ["r2e-img-", "pool.", "abc", "a1-b2."])
def test_template_name_prefix_accepts_valid_generated_name(good):
  cfg = FleetConfig(template_name_prefix=good)
  name = cfg.template_name("some/image:tag")
  dns1123 = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
  import re
  assert re.match(dns1123, name) and len(name) <= 253
