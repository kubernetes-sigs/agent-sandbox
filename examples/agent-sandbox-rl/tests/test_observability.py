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

import pytest

from agent_sandbox_rl import (
    ClusterRegistry,
    FleetConfig,
    ObservabilityConfig,
    Observer,
    RunReport,
    SandboxFleet,
    repo_family,
)
from agent_sandbox_rl.preflight import PreflightReport

prometheus_client = pytest.importorskip("prometheus_client")
REGISTRY = prometheus_client.REGISTRY


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _sample(name, labels):
  return REGISTRY.get_sample_value(name, labels) or 0.0


# --- repo_family ---------------------------------------------------------- #
def test_repo_family_from_metadata():
  t = types.SimpleNamespace(image="x", metadata={"repo": "django/django"})
  assert repo_family(t) == "django"


def test_repo_family_from_swebench_tag():
  img = "swebench/sweb.eval.x86_64.astropy__astropy-12907:latest"
  assert repo_family(img) == "astropy"


def test_repo_family_unknown():
  assert repo_family("busybox:latest") == "unknown"
  t = types.SimpleNamespace(image="busybox", metadata=None)
  assert repo_family(t) == "unknown"


# --- RunReport ------------------------------------------------------------ #
def test_runreport_aggregation():
  r = RunReport("naive")
  r.add_phase("claim", 0.2)
  r.add_phase("claim", 0.4)
  r.add_phase("process", 1.0)
  r.add_task("ok")
  r.add_task("error")
  r.add_claim()
  assert r.phases["claim"] == [2, pytest.approx(0.6), pytest.approx(0.4)]
  assert r.tasks_ok == 1 and r.tasks_err == 1
  assert r.claims == 1
  d = r.to_dict()
  assert d["phases"]["claim"]["count"] == 2
  assert d["tasks_err"] == 1


def test_runreport_environment_in_summary_and_dict():
  r = RunReport("naive")
  r.environment = {"rl": {"context": "ctx-a", "namespace": "ns",
                          "k8s_version": "v1.30.1", "nodes": 8,
                          "node_pools": ["e2-pool"],
                          "instance_types": ["e2-standard-4"],
                          "region": "us-central2"}}
  d = r.to_dict()
  assert d["environment"]["rl"]["nodes"] == 8
  s = r.summary()
  assert "environment:" in s
  assert "e2-pool" in s and "us-central2" in s and "v1.30.1" in s


def test_runreport_summary_orders_known_phases():
  r = RunReport("naive")
  r.add_phase("teardown", 0.5)
  r.add_phase("preflight", 0.1)
  s = r.summary()
  # preflight is listed before teardown despite insertion order
  assert s.index("preflight") < s.index("teardown")
  assert "TOTAL" in s


# --- Observer metrics ----------------------------------------------------- #
def test_observer_records_metrics():
  obs = Observer(ObservabilityConfig(enable_metrics=True, enable_tracing=False))
  assert obs.metrics is True
  before_claim = _sample("asrl_claims_total", {"cluster": "c1", "status": "ok"})
  before_task = _sample("asrl_tasks_total", {"strategy": "naive", "status": "ok"})
  before_phase = _sample(
      "asrl_phase_latency_seconds_count",
      {"phase": "claim", "cluster": "c1", "family": "django",
       "strategy": "naive", "status": "ok"})

  with obs.run("naive") as report:
    with obs.phase("claim", cluster="c1", family="django"):
      pass
    obs.claim("c1", "ok")
    obs.task_done("c1", "django", "ok", 0.3)

  assert report.claims == 1
  assert report.tasks_ok == 1
  assert "claim" in report.phases
  assert _sample("asrl_claims_total", {"cluster": "c1", "status": "ok"}) == before_claim + 1
  assert _sample("asrl_tasks_total", {"strategy": "naive", "status": "ok"}) == before_task + 1
  assert _sample(
      "asrl_phase_latency_seconds_count",
      {"phase": "claim", "cluster": "c1", "family": "django",
       "strategy": "naive", "status": "ok"}) == before_phase + 1


def test_observer_warm_gauge_and_peak():
  obs = Observer(ObservabilityConfig())
  with obs.run("naive") as report:
    obs.warm_add("c1", 3)
    obs.warm_add("c2", 2)        # peak 5
    obs.warm_remove("c1", 3)     # back to 2
    assert _sample("asrl_warm_replicas", {"cluster": "c2"}) == 2
  assert report.peak_warm == 5
  assert report.warm_total == 5
  obs.warm_reset()
  assert _sample("asrl_warm_replicas", {"cluster": "c2"}) == 0


def test_observer_phase_error_status():
  obs = Observer(ObservabilityConfig())
  before = _sample(
      "asrl_phase_latency_seconds_count",
      {"phase": "boom", "cluster": "-", "family": "-",
       "strategy": "naive", "status": "error"})
  with obs.run("naive"):
    with pytest.raises(RuntimeError):
      with obs.phase("boom"):
        raise RuntimeError("x")
  assert _sample(
      "asrl_phase_latency_seconds_count",
      {"phase": "boom", "cluster": "-", "family": "-",
       "strategy": "naive", "status": "error"}) == before + 1


# --- disabled path -------------------------------------------------------- #
def test_disabled_metrics_is_noop():
  obs = Observer(ObservabilityConfig(enable_metrics=False, enable_tracing=False))
  assert obs.metrics is False
  with obs.run("naive") as report:
    with obs.phase("claim", cluster="c1", family="f"):
      pass
    obs.claim("c1", "ok")
    obs.task_done("c1", "f", "ok", 0.1)
    obs.warm_add("c1", 1)
  # RunReport is always on even when metrics are disabled
  assert report.claims == 1
  assert report.tasks_ok == 1
  assert "claim" in report.phases


# --- serve_metrics smoke -------------------------------------------------- #
def test_serve_metrics_returns_server():
  from agent_sandbox_rl import serve_metrics
  server, thread = serve_metrics(port=0)
  try:
    assert server.server_port > 0
  finally:
    server.shutdown()


# --- end-to-end through the fleet ----------------------------------------- #
def test_fleet_run_populates_report(two_cluster_registry):
  f = SandboxFleet(FleetConfig(placement="round-robin"), registry=two_cluster_registry)
  f.load_tasks(["imgA", "imgB", "imgA"])
  res = f.run(lambda t, h: h.pod_name, strategy="naive")
  assert len(res) == 3
  rep = f.report
  assert rep is not None
  assert rep.tasks_ok == 3
  assert rep.claims == 3
  assert "process" in rep.phases
  assert rep.total_s >= 0.0


def test_import_has_no_metrics_side_effect():
  # In a fresh process, merely importing the package must NOT register any
  # asrl_* collectors on the global default registry (they're created lazily by
  # the first metrics-enabled Observer). Run in a subprocess so other tests in
  # this process that build Observers can't pre-register and mask a regression.
  import subprocess
  import sys
  code = (
      "import agent_sandbox_rl, agent_sandbox_rl.observability as o\n"
      "from prometheus_client import REGISTRY\n"
      "names = [n for n in REGISTRY._names_to_collectors if n.startswith('asrl_')]\n"
      "assert not names, names\n"
      "assert o._METRICS_READY is False\n"
      "from agent_sandbox_rl import ObservabilityConfig, Observer\n"
      "Observer(ObservabilityConfig(enable_metrics=True, enable_tracing=False))\n"
      "assert o._METRICS_READY is True\n"
      "assert any(n.startswith('asrl_') for n in REGISTRY._names_to_collectors)\n"
  )
  subprocess.run([sys.executable, "-c", code], check=True)


def test_metric_duplicate_registration_reuses():
  from agent_sandbox_rl import observability as o
  if not o._PROM:
    import pytest as _pytest
    _pytest.skip("prometheus_client not installed")
  from prometheus_client import Counter
  a = o._metric(Counter, "asrl_test_dup_total", "dup test", ["l"])
  b = o._metric(Counter, "asrl_test_dup_total", "dup test", ["l"])
  assert a is b      # second call reuses the existing collector, no ValueError
