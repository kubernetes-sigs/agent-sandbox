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
"""teardown(verify=True): the run-scoped reaper confirms and finishes a teardown."""
import sys
import types

import pytest

from agent_sandbox_rl import ClusterRegistry, FleetConfig, SandboxFleet
from agent_sandbox_rl.preflight import PreflightReport


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _patch_reap(monkeypatch, counts=None, error=None):
  calls = []
  def fake(run_id=None, **kw):
    calls.append((run_id, kw))
    if error:
      raise error
    return dict(counts or {"claims": 0, "warmpools": 0, "sandboxes": 0, "templates": 0,
                           "pods": "requested (force)"})
  monkeypatch.setattr(sys.modules["agent_sandbox_rl.reaper"], "reap", fake)
  return calls


def _fleet(c, **cfg):
  attrs = {"weight": 1.0, "max_replicas": None, **vars(c.config),
           "context": "ctx-1", "kubeconfig": "/kc", "in_cluster": False}
  c.config = types.SimpleNamespace(**attrs)
  f = SandboxFleet(FleetConfig(**cfg), registry=ClusterRegistry([c]))
  f.load_tasks(["img"])
  f.setup()
  return f


def test_teardown_without_verify_is_unchanged(make_cluster, monkeypatch):
  calls = _patch_reap(monkeypatch)
  f = _fleet(make_cluster("solo"))
  assert f.teardown() is None and calls == []


def test_verify_runs_the_run_scoped_reaper_per_cluster_and_reports(make_cluster, monkeypatch, caplog):
  calls = _patch_reap(monkeypatch, counts={"claims": 0, "warmpools": 1, "sandboxes": 2,
                                           "templates": 0, "pods": "requested (force)"})
  c = make_cluster("solo")
  f = _fleet(c)
  with caplog.at_level("WARNING"):
    out = f.teardown(verify=True)
  assert out == {"solo": {"claims": 0, "warmpools": 1, "sandboxes": 2, "templates": 0,
                          "pods": "requested (force)"}}
  (run_id, kw), = calls
  assert run_id == f.run_id
  assert kw == {"context": "ctx-1", "namespace": c.namespace, "kubeconfig": "/kc", "in_cluster": False}
  assert any("still had" in r.message and "warmpools" in r.message for r in caplog.records)
  assert f.verify_teardown()["solo"]["sandboxes"] == 2          # callable on its own


def test_verify_is_skipped_for_shared_run_ids_adopt_mode_and_deleted_namespaces(make_cluster, monkeypatch):
  calls = _patch_reap(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(c, run_id="shared-job")
  assert "shared run id" in f.teardown(verify=True)["solo"]["skipped"]
  g = _fleet(make_cluster("solo"))
  g.config = g.config.model_copy(update={"adopt_existing": True})
  assert "adopt_existing" in g.verify_teardown()["solo"]["skipped"]
  h = _fleet(make_cluster("solo"))
  h._deleted_namespaces.add(("solo", h.registry.get("solo").namespace))
  assert "was deleted" in h.verify_teardown()["solo"]["skipped"]
  assert calls == []


def test_verify_records_a_reaper_error_instead_of_raising(make_cluster, monkeypatch):
  _patch_reap(monkeypatch, error=RuntimeError("apiserver down"))
  f = _fleet(make_cluster("solo"))
  out = f.teardown(verify=True)
  assert out["solo"] == {"error": "RuntimeError: apiserver down"}


def test_teardown_records_the_namespaces_it_deleted(make_cluster, monkeypatch):
  _patch_reap(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(c)
  f.teardown(delete_namespace=True)
  assert ("solo", c.namespace) in f._deleted_namespaces
  assert "was deleted" in f.verify_teardown()["solo"]["skipped"]
