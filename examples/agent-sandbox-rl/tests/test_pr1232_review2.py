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

"""Regression tests for the second PR #1232 review round:
- a pre-existing (stale-run) template has its labels reconciled to this run;
- a caller-supplied registry still gets the run-id on its cluster labels;
- the circuit breaker trips only after N *consecutive* over-ceiling polls;
- reap() refuses the all-managed sweep unless it's opt-in.
"""

import time
from unittest.mock import MagicMock

import pytest

import agent_sandbox_rl.reaper as reaper
from agent_sandbox_rl import (ClusterRegistry, FleetConfig, SandboxFleet, constants)
from agent_sandbox_rl.config import TemplateSpec
from agent_sandbox_rl.exceptions import FleetOvercommitError
from agent_sandbox_rl.resources import Resources

IMG = "reg/swebench-verified@sha256:deadbeef"
TNAME = "r2e-img-abc123"
RID = constants.RUN_ID_LABEL


# --- template label reconciliation (blocking) ----------------------------- #
def test_ensure_template_reconciles_stale_run_label():
  r = Resources(MagicMock(), MagicMock(), "ns", labels={RID: "new"})
  # a leftover template from a prior run carries the OLD run-id
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": {constants.MANAGED_BY_LABEL: constants.MANAGED_BY_VALUE, RID: "old"}},
      "spec": {"podTemplate": {"metadata": {"labels": {"sandbox": TNAME, RID: "old"}}}},
  }
  created = r.ensure_template(IMG, TNAME, TemplateSpec())
  assert created is False
  r.custom_api.patch_namespaced_custom_object.assert_called_once()
  body = r.custom_api.patch_namespaced_custom_object.call_args.kwargs["body"]
  assert body["metadata"]["labels"][RID] == "new"
  assert body["spec"]["podTemplate"]["metadata"]["labels"][RID] == "new"
  assert body["spec"]["podTemplate"]["metadata"]["labels"]["sandbox"] == TNAME


def test_ensure_template_no_patch_when_labels_current():
  r = Resources(MagicMock(), MagicMock(), "ns", labels={RID: "same"})
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": dict(r.labels)},
      "spec": {"podTemplate": {"metadata": {"labels": {**r.labels, "sandbox": TNAME}}}},
  }
  r.ensure_template(IMG, TNAME, TemplateSpec())
  r.custom_api.patch_namespaced_custom_object.assert_not_called()


# --- caller-supplied registry gets the run-id (blocking, related gap) ------ #
def test_supplied_registry_clusters_get_run_id(make_cluster):
  c = make_cluster("solo")
  c.resources.labels = {constants.MANAGED_BY_LABEL: constants.MANAGED_BY_VALUE}  # built pre-run-id
  f = SandboxFleet(FleetConfig(install_teardown_hooks=False),
                   registry=ClusterRegistry([c]))
  assert c.resources.labels.get(RID) == f.run_id   # stamped onto the supplied registry


# --- breaker: sustained vs transient breach ------------------------------- #
def _breaker_fleet(make_cluster, pods, trip_polls):
  c = make_cluster("solo")
  if callable(pods):
    c.resources.count_pods.side_effect = lambda **k: pods()
  else:
    c.resources.count_pods.return_value = pods
  f = SandboxFleet(FleetConfig(overcommit_factor=1.0, breaker_poll_s=0.02,
                               breaker_trip_polls=trip_polls, install_teardown_hooks=False),
                   registry=ClusterRegistry([c]))
  f.teardown = lambda *a, **k: None   # no-op (avoid touching the fake registry)
  return f


def test_breaker_trips_on_sustained_breach(make_cluster):
  f = _breaker_fleet(make_cluster, pods=100, trip_polls=2)   # 100 >> ceiling(=1)
  with pytest.raises(FleetOvercommitError):
    with f.overcommit_guard(expected=1):
      time.sleep(0.25)                                       # ~12 polls → ≥2 consecutive → trip


def test_breaker_ignores_single_transient_spike(make_cluster):
  seq = iter([100])                                          # one breach, then healthy forever
  f = _breaker_fleet(make_cluster, pods=lambda: next(seq, 0), trip_polls=3)
  with f.overcommit_guard(expected=1):                      # 1 breach resets → never 3 consecutive
    time.sleep(0.2)
  # no FleetOvercommitError == pass


def test_breaker_trip_polls_default():
  assert FleetConfig().breaker_trip_polls == 3


# --- reaper all-managed is opt-in ----------------------------------------- #
def test_reap_refuses_all_managed_without_opt_in():
  with pytest.raises(ValueError):
    reaper.reap(run_id=None)                                 # no run_id, no all_managed → error


def test_reap_all_managed_opt_in_passes_guard(monkeypatch):
  # all_managed=True must get past the guard (we stub the cluster so no real k8s).
  fake_cluster = MagicMock()
  fake_cluster.resources.list_claims.return_value = []
  fake_cluster.resources.list_warmpools.return_value = []
  fake_cluster.resources.list_sandboxes.return_value = []
  fake_cluster.resources.list_templates.return_value = []
  monkeypatch.setattr(reaper, "Cluster", lambda *a, **k: fake_cluster)
  counts = reaper.reap(all_managed=True, delete_pods=False)  # must NOT raise
  assert isinstance(counts, dict)
