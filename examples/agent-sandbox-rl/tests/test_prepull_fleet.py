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
"""fleet.prepull wiring: template tolerations and the prepull_* config fields."""
import sys

import pytest

from agent_sandbox_rl import ClusterRegistry, FleetConfig, SandboxFleet, TemplateSpec
from agent_sandbox_rl.preflight import PreflightReport


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def test_fleet_prepull_passes_template_tolerations_and_config(make_cluster, monkeypatch):
  c = make_cluster("solo")
  tol = [{"key": "workload", "operator": "Equal", "value": "sandbox", "effect": "NoSchedule"}]
  f = SandboxFleet(FleetConfig(
      template=TemplateSpec(node_selector={"pool": "sbx"}, extra_pod_spec={"tolerations": tol}),
      prepull_images_per_pod=10, prepull_max_parallel=2, prepull_delete_when_done=True,
      prepull_drop_caches=True), registry=ClusterRegistry([c]))
  f.load_tasks(["a", "b"])
  f.plan()
  seen = {}
  def fake(cluster, images, **kw):
    seen["cluster"], seen["images"], seen["kw"] = cluster, images, kw
    return True
  # the package re-exports the function under the module's name: patch the module
  monkeypatch.setattr(sys.modules["agent_sandbox_rl.prepull"], "prepull", fake)
  assert f.prepull(wait=False, timeout=7) is True
  assert seen["cluster"] is c and sorted(seen["images"]) == ["a", "b"]
  kw = seen["kw"]
  assert kw["tolerations"] == tol and kw["node_selector"] == {"pool": "sbx"}
  assert kw["runtime_class"] is None
  assert (kw["images_per_pod"], kw["max_parallel"], kw["delete_when_done"],
          kw["drop_caches"], kw["wait"], kw["timeout"]) == (10, 2, True, True, False, 7)
  # an override of a template-derived value wins instead of raising TypeError
  assert f.prepull(wait=False, tolerations=[{"key": "other"}], runtime_class="gvisor") is True
  assert seen["kw"]["tolerations"] == [{"key": "other"}] and seen["kw"]["runtime_class"] == "gvisor"
  deleted = {}
  monkeypatch.setattr(sys.modules["agent_sandbox_rl.prepull"], "prepull_delete",
                      lambda cluster, labels=None: deleted.setdefault("labels", labels))
  f.prepull_delete()
  assert deleted["labels"] == f.config.labels


def test_setup_warns_when_the_prepull_budget_runs_out(make_cluster, monkeypatch, caplog):
  c = make_cluster("solo")
  f = SandboxFleet(FleetConfig(template=TemplateSpec(runtime_class="gvisor")),
                   registry=ClusterRegistry([c]))
  f.load_tasks(["a"])
  seen = {}
  def fake(cluster, images, **kw):
    seen.update(kw)
    return False
  monkeypatch.setattr(sys.modules["agent_sandbox_rl.prepull"], "prepull", fake)
  with caplog.at_level("WARNING", logger="agent_sandbox_rl.fleet"):
    f.setup(prepull=True)
  assert seen["runtime_class"] == "gvisor"
  assert any("pre-pull did not finish" in r.message for r in caplog.records)
