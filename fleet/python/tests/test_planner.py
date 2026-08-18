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

from __future__ import annotations

import logging

import pytest

from agent_sandbox_fleet.placement import PlannerCluster, PlannerRegistry
from agent_sandbox_fleet.planner import (
    Assignments,
    ClusterAssignment,
    FleetSpec,
    ModelSpec,
    plan,
)


def _spec(**overrides):
  base = dict(
      generation=1,
      max_concurrent=10,
      max_pool=20,
      placement_policy="capacity-aware",
      cluster_weights={"a": 1.0, "b": 1.0},
      models=[
          ModelSpec(image="gcr.io/x/img-1:v1", template_name="tmpl-1",
                    target_tasks=5),
          ModelSpec(image="gcr.io/x/img-2:v1", template_name="tmpl-2",
                    target_tasks=5),
      ],
  )
  base.update(overrides)
  return FleetSpec(**base)


def _registry() -> PlannerRegistry:
  reg = PlannerRegistry()
  reg.clusters["a"] = PlannerCluster(name="a", report_age_s=1)
  reg.clusters["b"] = PlannerCluster(name="b", report_age_s=1)
  return reg


def test_replicas_sum_at_most_max_concurrent():
  spec = _spec()
  assn = plan(spec, _registry())
  total = sum(p.replicas for c in assn.clusters.values() for p in c.pools)
  # Concurrency-proportional sizing caps per-pool by tasks_image and max_pool,
  # so total is <= max_concurrent (may be slightly less due to per-pool round).
  assert total <= spec.max_concurrent


def test_every_cluster_appears_in_output():
  spec = _spec()
  assn = plan(spec, _registry())
  # Even a cluster with no pools should be represented (empty pools = "drop everything")
  assert set(assn.clusters) == {"a", "b"}


def test_pool_names_derive_from_template():
  spec = _spec()
  assn = plan(spec, _registry())
  all_pools = [p for c in assn.clusters.values() for p in c.pools]
  for p in all_pools:
    assert p.warmpool == f"{p.template}-pool"


def test_generation_propagates():
  assn = plan(_spec(generation=42), _registry())
  assert assn.generation == 42


def test_single_cluster_gets_everything():
  reg = PlannerRegistry()
  reg.clusters["solo"] = PlannerCluster(name="solo", report_age_s=1)
  spec = _spec(cluster_weights={"solo": 1.0})
  assn = plan(spec, reg)
  assert set(assn.clusters) == {"solo"}
  # Both models placed on the solo cluster
  assert len(assn.clusters["solo"].pools) == 2


# --------------------------------------------------------------------------- #
# v1.5 spread-first pre-pass regression tests.
# --------------------------------------------------------------------------- #

def _spec_with_n_models(n: int, cluster_weights: dict[str, float]):
  """FleetSpec with n models, each a distinct template."""
  return FleetSpec(
      generation=1, max_concurrent=100, max_pool=50,
      placement_policy="capacity-aware", cluster_weights=cluster_weights,
      models=[
          ModelSpec(image=f"img-{i}:v1", template_name=f"tmpl-{i}",
                    target_tasks=20)
          for i in range(n)
      ],
  )


def _n_fresh_clusters(n: int) -> PlannerRegistry:
  reg = PlannerRegistry()
  for i in range(n):
    reg.clusters[f"c{i}"] = PlannerCluster(name=f"c{i}", report_age_s=1)
  return reg


def test_spread_first_uses_all_clusters_when_models_ge_clusters():
  """5 models + 5 fresh clusters → each cluster gets ONE pool.

  This is the regression case for the placement-oscillation fix. Without
  spread-first, a stale wp_depth on one cluster would cause it to be
  skipped and 2 models would double up on another.
  """
  reg = _n_fresh_clusters(5)
  # Simulate the failure mode: one cluster looks slightly loaded at plan
  # time (would previously be skipped by pure-greedy CapacityAware).
  reg.clusters["c2"].warmpool_depth = 30
  reg.clusters["c2"].warmpool_ready = 10
  spec = _spec_with_n_models(5, {f"c{i}": 1.0 for i in range(5)})
  assn = plan(spec, reg)
  # Every cluster must have exactly one pool despite c2's leftover load.
  for cname in [f"c{i}" for i in range(5)]:
    assert len(assn.clusters[cname].pools) == 1, (
        f"{cname} pools={[p.warmpool for p in assn.clusters[cname].pools]}")


def test_spread_first_then_scored_for_extras():
  """When models > clusters, first N models spread; remaining use selector."""
  reg = _n_fresh_clusters(3)
  spec = _spec_with_n_models(5, {f"c{i}": 1.0 for i in range(3)})
  assn = plan(spec, reg)
  # 5 pools total, 3 clusters. First 3 models spread one-per-cluster,
  # remaining 2 go via CapacityAware.
  total_pools = sum(len(a.pools) for a in assn.clusters.values())
  assert total_pools == 5
  for cname in [f"c{i}" for i in range(3)]:
    assert len(assn.clusters[cname].pools) >= 1, (
        f"{cname} got 0 pools; spread-first violated")


def test_spread_first_deterministic_by_name_order():
  """Spread-first pins models to clusters in sorted-name order."""
  reg = _n_fresh_clusters(3)
  spec = _spec_with_n_models(3, {f"c{i}": 1.0 for i in range(3)})
  assn = plan(spec, reg)
  # tmpl-0 → c0, tmpl-1 → c1, tmpl-2 → c2 (both sorted alphabetically).
  assert assn.clusters["c0"].pools[0].template == "tmpl-0"
  assert assn.clusters["c1"].pools[0].template == "tmpl-1"
  assert assn.clusters["c2"].pools[0].template == "tmpl-2"


def test_spread_first_skips_stale_clusters():
  """Stale clusters (report_age_s > threshold) are not counted for spread."""
  reg = PlannerRegistry()
  reg.clusters["fresh1"] = PlannerCluster(name="fresh1", report_age_s=1)
  reg.clusters["fresh2"] = PlannerCluster(name="fresh2", report_age_s=1)
  reg.clusters["stale"] = PlannerCluster(name="stale", report_age_s=1e9)
  spec = _spec_with_n_models(2, {"fresh1": 1.0, "fresh2": 1.0, "stale": 1.0})
  assn = plan(spec, reg)
  # Only fresh clusters get pools; stale cluster shows up with empty pools.
  assert len(assn.clusters["fresh1"].pools) == 1
  assert len(assn.clusters["fresh2"].pools) == 1
  assert len(assn.clusters["stale"].pools) == 0


# --------------------------------------------------------------------------- #
# v1.5 min_clusters (anti-affinity) tests.
# --------------------------------------------------------------------------- #

def test_min_clusters_round_robin_more_models_than_clusters():
  """8 models × 5 fresh clusters with min_clusters=5 → 8 pools spread as
  2/2/2/1/1 (or permutation). No cluster misses out; extras distributed
  round-robin, not via scored placement."""
  reg = _n_fresh_clusters(5)
  spec = _spec_with_n_models(8, {f"c{i}": 1.0 for i in range(5)})
  spec = spec.model_copy(update={"min_clusters": 5})
  assn = plan(spec, reg)
  counts = sorted(len(assn.clusters[f"c{i}"].pools) for i in range(5))
  # 8 pools across 5 clusters round-robin → three 2s and two 1s.
  assert counts == [1, 1, 2, 2, 2], f"expected [1,1,2,2,2], got {counts}"


def test_min_clusters_deterministic_across_state_changes():
  """min_clusters mode must be deterministic — same spec + same fresh set →
  same placement regardless of pre-existing load. This is what kills the
  CapacityAware ping-pong."""
  reg1 = _n_fresh_clusters(3)
  reg2 = _n_fresh_clusters(3)
  # Simulate one cluster having load in reg2 (the state that would flip
  # CapacityAware placement).
  reg2.clusters["c1"].warmpool_depth = 50
  reg2.clusters["c1"].warmpool_ready = 50
  spec = _spec_with_n_models(6, {f"c{i}": 1.0 for i in range(3)})
  spec = spec.model_copy(update={"min_clusters": 3})
  a1 = plan(spec, reg1)
  a2 = plan(spec, reg2)
  # Placement should be identical despite different live state.
  for cn in ["c0", "c1", "c2"]:
    t1 = sorted(p.template for p in a1.clusters[cn].pools)
    t2 = sorted(p.template for p in a2.clusters[cn].pools)
    assert t1 == t2, f"{cn}: state-dependent placement (t1={t1}, t2={t2})"


def test_min_clusters_zero_uses_default_spread_first():
  """min_clusters=0 (default) keeps the existing spread-first + scored path."""
  reg = _n_fresh_clusters(5)
  spec = _spec_with_n_models(5, {f"c{i}": 1.0 for i in range(5)})
  # min_clusters defaults to 0 — same as before.
  assn = plan(spec, reg)
  for cname in [f"c{i}" for i in range(5)]:
    assert len(assn.clusters[cname].pools) == 1


def test_min_clusters_capped_by_available_fresh():
  """If min_clusters > fresh_clusters, we use what we have."""
  reg = _n_fresh_clusters(3)
  spec = _spec_with_n_models(6, {f"c{i}": 1.0 for i in range(3)})
  spec = spec.model_copy(update={"min_clusters": 10})  # ask for 10, have 3
  assn = plan(spec, reg)
  # 6 pools across 3 clusters round-robin → 2 per cluster.
  counts = sorted(len(assn.clusters[f"c{i}"].pools) for i in range(3))
  assert counts == [2, 2, 2]


# --------------------------------------------------------------------------- #
# max_concurrent is a TARGET, not a hard cap.
#
# sizing.compute_replicas floors every placed pool at 1 replica — a template
# assigned 0 replicas can only be served cold. When a cluster holds more
# models than its budget slice, the floor wins. That is intended; the point of
# these tests is that the overshoot is bounded, attributable, and logged
# rather than silent.
# --------------------------------------------------------------------------- #

def test_min_one_replica_floor_can_exceed_the_budget(caplog):
  # 6 models, budget 2, all on one cluster: the floor gives 6 replicas.
  spec = _spec(
      max_concurrent=2,
      cluster_weights={"a": 1.0},
      models=[ModelSpec(image=f"img-{i}", template_name=f"tmpl-{i}",
                        target_tasks=10) for i in range(6)],
  )
  reg = PlannerRegistry()
  reg.clusters["a"] = PlannerCluster(name="a", report_age_s=1)

  with caplog.at_level(logging.WARNING):
    assn = plan(spec, reg)

  pools = assn.clusters["a"].pools
  total = sum(p.replicas for p in pools)
  assert all(p.replicas >= 1 for p in pools), "a placed pool must be warmable"
  assert total == 6
  # Bounded by the floor, not unbounded: one per pool, no more.
  assert total == len(pools)
  assert "exceeds its budget slice" in caplog.text


def test_no_warning_when_the_budget_is_respected(caplog):
  with caplog.at_level(logging.WARNING):
    plan(_spec(), _registry())
  assert "exceeds its budget slice" not in caplog.text


# --------------------------------------------------------------------------- #
# Stale clusters are assigned empty, which DROPS their pools. Intended (a
# drain has to be able to empty a cluster that is not reporting), but the
# trigger is a missing capacity report, not a missing member — so it must be
# logged rather than silent.
# --------------------------------------------------------------------------- #

def test_stale_cluster_is_emptied_and_the_teardown_is_logged(caplog):
  reg = PlannerRegistry()
  reg.clusters["a"] = PlannerCluster(name="a", report_age_s=1)
  reg.clusters["gone"] = PlannerCluster(
      name="gone", report_age_s=reg.max_report_age_s + 60)

  with caplog.at_level(logging.WARNING):
    assn = plan(_spec(cluster_weights={"a": 1.0, "gone": 1.0}), reg)

  assert assn.clusters["gone"].pools == []
  assert assn.clusters["a"].pools, "fresh cluster still gets the models"
  assert "DROPS any warm pools" in caplog.text


def test_fresh_cluster_with_no_models_is_emptied_quietly(caplog):
  # Nothing placed on "b" (only one model, spread-first puts it on "a"), but
  # "b" is reporting fine — that is not a teardown warning.
  reg = _registry()
  spec = _spec(models=[ModelSpec(image="img-1", template_name="tmpl-1",
                                 target_tasks=5)])
  with caplog.at_level(logging.WARNING):
    assn = plan(spec, reg)
  assert assn.clusters["b"].pools == []
  assert "DROPS any warm pools" not in caplog.text


# --------------------------------------------------------------------------- #
# apply() must not leave the bucket describing a plan that never happened.
# --------------------------------------------------------------------------- #

class _RecordingGCS:
  """Records put_json in call order. Enough surface for planner.apply."""

  def __init__(self):
    self.puts: list[str] = []

  @property
  def bucket_name(self):
    return "fake-bucket"

  def put_json(self, path, obj):
    self.puts.append(path)

  def list_prefix(self, prefix):
    return []

  def get_json(self, path):
    return None


class _StaticProvider:
  def __init__(self, registry):
    self._registry = registry

  def load(self, weights):
    return self._registry


def _one_model_spec():
  return _spec(generation=9)


def test_apply_does_not_persist_the_spec_when_planning_fails():
  # plan() raises NoClusterAvailableError when no cluster is fresh. Writing
  # spec.json first left the bucket holding generation 9 with assignments.json
  # still on the previous generation -- and show-registry reads cluster_weights
  # out of that spec, so the fleet was then described by a plan nobody applied.
  from agent_sandbox_fleet import planner
  from agent_sandbox_fleet.placement import NoClusterAvailableError, PlannerRegistry

  gcs = _RecordingGCS()
  with pytest.raises(NoClusterAvailableError):
    planner.apply(gcs, _one_model_spec(), provider=_StaticProvider(PlannerRegistry()))
  assert gcs.puts == [], "spec.json was written for a plan that never happened"


def test_apply_writes_the_spec_once_planning_has_succeeded():
  # The other half: on the happy path both objects still land, and the spec
  # is not accidentally skipped by the reordering.
  from agent_sandbox_fleet import planner
  from agent_sandbox_fleet.objectstore import Paths
  from agent_sandbox_fleet.placement import PlannerCluster, PlannerRegistry

  reg = PlannerRegistry()
  reg.clusters["a"] = PlannerCluster(name="a", report_age_s=0.0)
  gcs = _RecordingGCS()
  planner.apply(gcs, _one_model_spec(), provider=_StaticProvider(reg))
  paths = Paths()
  assert set(gcs.puts) == {paths.spec, paths.assignments}
