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

from agent_sandbox_fleet import planner
from agent_sandbox_fleet.objectstore import CASConflict, Paths
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


def test_generation_comes_from_the_caller_not_the_spec():
    # plan() is pure: deriving the generation needs a read of the published
    # assignments, which is apply()'s job. See test_apply_derives_* below.
    assn = plan(_spec(), _registry(), generation=42)
    assert assn.generation == 42
    assert assn.schema_version == planner.SCHEMA_VERSION


def test_a_spec_authored_generation_is_ignored_and_warned_about(caplog):
    # It used to be the source of truth, and forgetting to bump it was a silent
    # no-op apply. Dropping the field outright would be silent too -- pydantic
    # ignores extras -- so it is kept, normalised away, and warned about.
    with caplog.at_level(logging.WARNING):
        spec = _spec(generation=9)
    assert spec.generation is None
    assert "deprecated and IGNORED" in caplog.text


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
        max_concurrent=100, max_pool=50,
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
    """Records put_json in call order. Enough surface for planner.apply.

    Also models the store's own generation counter, because apply()'s write is
    now conditional on it: one increment per successful write, and a mismatched
    precondition raises CASConflict exactly as GCS does.
    """

    def __init__(self):
        self.puts: list[str] = []
        self.objects: dict[str, object] = {}
        self.generations: dict[str, int] = {}

    @property
    def bucket_name(self):
        return "fake-bucket"

    def put_json(self, path, obj, if_generation_match=None):
        if if_generation_match is not None:
            if if_generation_match != self.generations.get(path, 0):
                raise CASConflict(path)
        self.puts.append(path)
        self.objects[path] = obj
        self.generations[path] = self.generations.get(path, 0) + 1

    def list_prefix(self, prefix):
        return []

    def get_json(self, path):
        return self.objects.get(path)

    def get_json_with_generation(self, path):
        return self.objects.get(path), self.generations.get(path, 0)


class _StaticProvider:
    def __init__(self, registry):
        self._registry = registry

    def load(self, weights):
        return self._registry


def _one_model_spec():
    return _spec()


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
    from agent_sandbox_fleet.placement import PlannerCluster, PlannerRegistry

    reg = PlannerRegistry()
    reg.clusters["a"] = PlannerCluster(name="a", report_age_s=0.0)
    gcs = _RecordingGCS()
    planner.apply(gcs, _one_model_spec(), provider=_StaticProvider(reg))
    paths = Paths()
    assert set(gcs.puts) == {paths.spec, paths.assignments}


# --------------------------------------------------------------------------- #
# Drain: cluster_weights[x] = 0.
#
# Regression suite for the bug where a drained cluster still received work.
# Scoring alone never enforced the drain, because two of the three placement
# paths never call the scorer: the spread-first pre-pass and the min_clusters
# round-robin both assign positionally off the candidate list. A zero-weight
# cluster therefore got one pool per plan, Hamilton handed it a budget slice of
# 0, and sizing.compute_replicas floored that at 1 -- so draining a cluster
# started a sandbox on it. Eligibility is now filtered before placement.
# --------------------------------------------------------------------------- #

def _drainable_registry() -> PlannerRegistry:
    reg = PlannerRegistry()
    for name, w in [("keep-1", 1.0), ("keep-2", 1.0), ("drained", 0.0)]:
        reg.clusters[name] = PlannerCluster(name=name, weight=w, report_age_s=1)
    return reg


def _drain_spec(**overrides):
    base = dict(
        max_concurrent=300,
        max_pool=100,
        cluster_weights={"keep-1": 1.0, "keep-2": 1.0, "drained": 0.0},
        models=[ModelSpec(template_name=f"t{i}", target_tasks=100)
                for i in range(3)],
    )
    base.update(overrides)
    return _spec(**base)


def test_a_drained_cluster_gets_no_pools_from_the_spread_first_pre_pass():
    # 3 models, 3 clusters: spread-first would hand one to every cluster in
    # sorted-name order, drained included, without ever consulting the scorer.
    assn = plan(_drain_spec(), _drainable_registry())
    assert assn.clusters["drained"].pools == []
    placed = sorted(p.template for c in assn.clusters.values() for p in c.pools)
    assert placed == ["t0", "t1", "t2"], "every model must still be placed"


def test_a_drained_cluster_gets_no_pools_under_min_clusters():
    # min_clusters ignores scoring entirely and round-robins positionally, so
    # it needs the same filter. Ask for 3 with only 2 eligible.
    assn = plan(_drain_spec(min_clusters=3), _drainable_registry())
    assert assn.clusters["drained"].pools == []
    assert sum(len(c.pools) for c in assn.clusters.values()) == 3


def test_a_drained_cluster_is_still_present_and_empty_so_it_tears_down():
    # Absent would also work -- the member treats absent as empty -- but being
    # explicitly present and empty is what makes `fleetctl show-assignments`
    # show the drain rather than just omitting the cluster.
    assn = plan(_drain_spec(), _drainable_registry())
    assert "drained" in assn.clusters
    assert assn.clusters["drained"].pools == []


def test_a_drained_cluster_never_reaches_the_one_replica_floor():
    # The original symptom: budget slice 0, floored to 1, live pool on a
    # cluster the operator drained. Assert on replicas, not just pool count.
    assn = plan(_drain_spec(), _drainable_registry())
    assert sum(p.replicas for p in assn.clusters["drained"].pools) == 0


def test_draining_the_whole_fleet_plans_empty_rather_than_splitting_evenly():
    # budget.hamilton_split reads an all-zero weight map as "no preference,
    # split evenly", which is right for a caller with no ranking and an
    # inversion of intent here: it would turn a full drain into a full deploy.
    # Filtering eligibility upstream means the planner never hands it that map.
    reg = PlannerRegistry()
    for name in ["a", "b"]:
        reg.clusters[name] = PlannerCluster(name=name, weight=0.0, report_age_s=1)
    spec = _spec(cluster_weights={"a": 0.0, "b": 0.0})
    assn = plan(spec, reg)
    assert all(c.pools == [] for c in assn.clusters.values())
    assert set(assn.clusters) == {"a", "b"}


def test_no_fresh_report_raises_instead_of_planning_a_fleet_wide_teardown():
    # The opposite of a drain, and it must not be confused with one: an empty
    # candidate set because nothing REPORTED means the planner cannot see the
    # fleet. Publishing empty there would drop every warm pool everywhere in
    # response to a bucket blip. Only an explicit weight of 0 authorises that.
    from agent_sandbox_fleet.placement import NoClusterAvailableError

    reg = PlannerRegistry()
    reg.clusters["a"] = PlannerCluster(name="a", weight=1.0, report_age_s=1e9)
    with pytest.raises(NoClusterAvailableError):
        plan(_spec(cluster_weights={"a": 1.0}), reg)


def test_drain_is_logged_once_by_name(caplog):
    # A drain is invisible in the assignment (the cluster just has no pools),
    # so the plan has to say it excluded one and which.
    with caplog.at_level(logging.INFO, logger="agent_sandbox_fleet.planner"):
        plan(_drain_spec(), _drainable_registry())
    msgs = [r.getMessage() for r in caplog.records]
    assert any("drained" in m and "weight 0" in m for m in msgs), msgs


def test_an_unlisted_cluster_is_not_treated_as_drained():
    # weight defaults to 1.0 for a cluster absent from cluster_weights, so
    # "not mentioned" must stay eligible. If eligibility were ever keyed off
    # the weights map rather than the resolved weight, omitting a cluster
    # would silently drain it.
    reg = PlannerRegistry()
    reg.clusters["listed"] = PlannerCluster(name="listed", weight=1.0, report_age_s=1)
    reg.clusters["unlisted"] = PlannerCluster(name="unlisted", report_age_s=1)
    spec = _spec(cluster_weights={"listed": 1.0})
    assn = plan(spec, reg)
    assert assn.clusters["unlisted"].pools, "an unlisted cluster must stay eligible"


# --------------------------------------------------------------------------- #
# schema_version + derived generation + CAS.
#
# One integer used to answer three questions: "can I parse this", "is this
# newer", and "did someone else write since I read". They are now three fields
# with three owners -- the writer's code version, the planner, and the object
# store -- because conflating them made each failure look like the others.
# --------------------------------------------------------------------------- #

def _live_registry() -> PlannerRegistry:
    reg = PlannerRegistry()
    reg.clusters["a"] = PlannerCluster(name="a", report_age_s=0.0)
    return reg


def _apply(gcs, **kw):
    return planner.apply(gcs, _spec(), provider=_StaticProvider(_live_registry()),
                         **kw)


def test_the_first_apply_of_a_fleet_publishes_generation_one():
    # Nothing published yet: store generation 0, payload generation 0, so the
    # first plan is 1. No bootstrap special case anywhere in apply().
    gcs = _RecordingGCS()
    assert _apply(gcs).generation == 1


def test_apply_derives_the_next_generation_from_the_published_assignments():
    gcs = _RecordingGCS()
    assert [_apply(gcs).generation for _ in range(3)] == [1, 2, 3]


def test_the_derived_generation_ignores_a_stale_archived_spec():
    # The archive is a record, not the counter. Corrupt it and the next apply
    # must still advance off assignments.json -- this is the desync that made
    # deriving from the spec wrong.
    gcs = _RecordingGCS()
    _apply(gcs)
    _apply(gcs)
    gcs.objects[Paths().spec]["generation"] = 99
    assert _apply(gcs).generation == 3


def test_the_archived_spec_records_the_generation_that_was_applied():
    gcs = _RecordingGCS()
    _apply(gcs)
    assn = _apply(gcs)
    archived = gcs.objects[Paths().spec]
    assert archived["applied_generation"] == assn.generation == 2
    assert "generation" not in archived, (
        "stamping the deprecated input field makes every cmd_status run warn "
        "about a value apply() itself wrote"
    )


def test_revalidating_the_archive_does_not_trip_the_deprecation_warning(caplog):
    # cmd_status round-trips the archive through FleetSpec.model_validate, so
    # the archive must not carry the deprecated `generation` input field.
    gcs = _RecordingGCS()
    _apply(gcs)
    with caplog.at_level(logging.WARNING):
        planner.FleetSpec.model_validate(gcs.objects[Paths().spec])
    assert "deprecated" not in caplog.text


def test_assignments_are_published_before_the_spec_is_archived():
    # Archive last: it is the only write whose failure is survivable, because
    # nothing reads it back to make a decision.
    gcs = _RecordingGCS()
    _apply(gcs)
    paths = Paths()
    assert gcs.puts == [paths.assignments, paths.spec]


def test_a_concurrent_apply_loses_the_race_instead_of_overwriting():
    # Two admins derive the same next generation from the same base. Without a
    # precondition on the STORE's generation, the second write silently discards
    # the first plan and both operators see success.
    gcs = _RecordingGCS()
    _apply(gcs)
    published_gen, store_gen = planner.read_published(gcs, Paths())
    _apply(gcs)  # the winner moves the store generation on
    stale = plan(_spec(), _live_registry(),
                 generation=planner.next_generation(published_gen))
    with pytest.raises(CASConflict):
        planner.publish(gcs, stale, Paths(), if_generation_match=store_gen)


def test_an_explicit_generation_override_is_honoured():
    gcs = _RecordingGCS()
    _apply(gcs)
    assert _apply(gcs, generation=50).generation == 50


def test_an_override_that_does_not_advance_is_rejected():
    # The one failure in this system with no symptom: members ignore the plan
    # exactly as designed, nothing errors, and the fleet does not change.
    gcs = _RecordingGCS()
    _apply(gcs, generation=7)
    with pytest.raises(ValueError, match="not greater than"):
        _apply(gcs, generation=7)


def test_apply_refuses_to_overwrite_a_plan_it_cannot_parse():
    # A newer fleetctl published; this one must not clobber it, and must not
    # increment a generation it read out of a payload it does not understand.
    gcs = _RecordingGCS()
    _apply(gcs)
    gcs.objects[Paths().assignments]["schema_version"] = 999
    with pytest.raises(ValueError, match="schema_version"):
        _apply(gcs)


def test_a_spec_with_an_unknown_schema_version_is_rejected_at_load():
    with pytest.raises(Exception, match="schema_version"):
        _spec(schema_version=999)


def test_published_assignments_carry_the_schema_version():
    gcs = _RecordingGCS()
    _apply(gcs)
    assert gcs.objects[Paths().assignments]["schema_version"] == planner.SCHEMA_VERSION


# ---------------------------------------------------------------------------
# pinned placement (added 2026-08-28: pre-sharded fleets — SBD shard layouts)
# ---------------------------------------------------------------------------

def _pinned_spec(**overrides):
    base = dict(
        max_concurrent=10,
        max_pool=20,
        placement_policy="pinned",
        cluster_weights={"a": 1.0, "b": 1.0},
        models=[
            ModelSpec(image="gcr.io/x/img-1:v1", template_name="tmpl-1",
                      target_tasks=5, cluster="a"),
            ModelSpec(image="gcr.io/x/img-2:v1", template_name="tmpl-2",
                      target_tasks=5, cluster="b"),
            ModelSpec(image="gcr.io/x/img-3:v1", template_name="tmpl-3",
                      target_tasks=5, cluster="a"),
        ],
    )
    base.update(overrides)
    return FleetSpec(**base)


def test_pinned_places_each_model_on_its_named_cluster():
    assn = plan(_pinned_spec(), _registry())
    a_templates = {p.template for p in assn.clusters["a"].pools}
    b_templates = {p.template for p in assn.clusters["b"].pools}
    assert a_templates == {"tmpl-1", "tmpl-3"}
    assert b_templates == {"tmpl-2"}


def test_pinned_requires_a_cluster_on_every_model():
    spec = _pinned_spec()
    spec.models[1].cluster = None
    with pytest.raises(ValueError, match="requires a cluster on every model"):
        plan(spec, _registry())


def test_pinned_to_unknown_or_stale_cluster_fails_the_plan():
    spec = _pinned_spec()
    spec.models[0].cluster = "ghost"
    with pytest.raises(planner.placement.NoClusterAvailableError,
                       match="pinned"):
        plan(spec, _registry())


def test_pinned_to_drained_cluster_fails_the_plan():
    reg = _registry()
    reg.clusters["a"].weight = 0  # drained
    with pytest.raises(planner.placement.NoClusterAvailableError):
        plan(_pinned_spec(), reg)


def test_pinned_wins_over_min_clusters():
    # An explicit pin is a stronger statement than the anti-affinity floor.
    spec = _pinned_spec(min_clusters=2)
    spec.models = [ModelSpec(image="gcr.io/x/i:v", template_name="only",
                             target_tasks=5, cluster="a")]
    assn = plan(spec, _registry())
    assert [p.template for p in assn.clusters["a"].pools] == ["only"]
    assert assn.clusters["b"].pools == []


def test_pinned_sizing_exact_when_budget_matches_totals():
    # The pre-sharded recipe: cluster_weights = per-cluster task totals,
    # max_concurrent = fleet total, max_pool = replicas-per-pool. Hamilton
    # then hands every cluster exactly its task count and every pool sizes
    # to exactly target_tasks.
    spec = _pinned_spec(
        max_concurrent=15,
        max_pool=5,
        cluster_weights={"a": 10.0, "b": 5.0},
    )
    # In the real flow inventory.load() copies spec.cluster_weights onto the
    # registry; plan() reads REGISTRY weights, so mirror that here.
    reg = _registry()
    reg.clusters["a"].weight = 10.0
    reg.clusters["b"].weight = 5.0
    assn = plan(spec, reg)
    for c in ("a", "b"):
        for p in assn.clusters[c].pools:
            assert p.replicas == 5


def test_pinned_other_policies_ignore_the_cluster_field():
    spec = _pinned_spec(placement_policy="capacity-aware")
    assn = plan(spec, _registry())  # must not raise, pins are decorative here
    placed = sum(len(c.pools) for c in assn.clusters.values())
    assert placed == 3
