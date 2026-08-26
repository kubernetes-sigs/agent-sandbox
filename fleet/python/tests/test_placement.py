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

import pytest

from agent_sandbox_fleet.placement import (
    CapacityAware,
    ImageAffinity,
    LeastLoaded,
    NoClusterAvailableError,
    PlannerCluster,
    PlannerRegistry,
    RoundRobin,
    get_placement,
)


def _reg(*clusters: PlannerCluster) -> PlannerRegistry:
    reg = PlannerRegistry()
    for c in clusters:
        reg.clusters[c.name] = c
    return reg


def test_round_robin_rotates():
    reg = _reg(
        PlannerCluster(name="a", report_age_s=1),
        PlannerCluster(name="b", report_age_s=1),
    )
    rr = RoundRobin()
    picks = [rr.select("img", reg).name for _ in range(4)]
    assert picks == ["a", "b", "a", "b"]


def test_least_loaded_picks_least_claims():
    reg = _reg(
        PlannerCluster(name="a", active_claims=10, report_age_s=1),
        PlannerCluster(name="b", active_claims=2, report_age_s=1),
    )
    assert LeastLoaded().select("img", reg).name == "b"


def test_image_affinity_is_deterministic():
    reg = _reg(
        PlannerCluster(name="a", report_age_s=1),
        PlannerCluster(name="b", report_age_s=1),
        PlannerCluster(name="c", report_age_s=1),
    )
    aff = ImageAffinity()
    # Same image → same cluster across runs
    first = aff.select("gcr.io/foo/bar:v1", reg).name
    for _ in range(5):
        assert aff.select("gcr.io/foo/bar:v1", reg).name == first


def test_stale_reports_are_excluded():
    reg = _reg(
        PlannerCluster(name="fresh", report_age_s=10),
        PlannerCluster(name="stale", report_age_s=1000),
    )
    # `reg.fresh()` filters out stale
    assert [c.name for c in reg.fresh()] == ["fresh"]
    # Selectors iterate `reg` which delegates to `fresh()`
    assert RoundRobin().select("img", reg).name == "fresh"


def test_no_cluster_raises():
    reg = _reg(PlannerCluster(name="a", report_age_s=1000))  # stale
    with pytest.raises(NoClusterAvailableError):
        LeastLoaded().select("img", reg)


def test_capacity_aware_penalizes_pressure():
    reg = _reg(
        PlannerCluster(name="quiet", warmpool_depth=10, warmpool_ready=10,
                       node_pressure_score=0.1, report_age_s=1),
        PlannerCluster(name="loaded", warmpool_depth=10, warmpool_ready=10,
                       node_pressure_score=0.9, report_age_s=1,
                       active_claims=5),  # active_claims bumps load
    )
    # Note: active_replicas = warmpool_depth + planned_replicas, so both start
    # with load=10. The `loaded` one has higher pressure → lower score.
    assert CapacityAware().select("img", reg).name == "quiet"


def test_capacity_aware_falls_back_on_ties():
    # Two brand-new clusters, no signals — should delegate to LeastLoaded.
    reg = _reg(
        PlannerCluster(name="a", report_age_s=1),
        PlannerCluster(name="b", report_age_s=1),
    )
    # Both score identical → falls back to LeastLoaded → ties by (0,0), pick "a".
    picked = CapacityAware().select("img", reg).name
    assert picked in ("a", "b")


def test_registry_lookup_by_name():
    assert isinstance(get_placement("capacity-aware"), CapacityAware)
    assert isinstance(get_placement("round-robin"), RoundRobin)
    with pytest.raises(ValueError):
        get_placement("nonsense")


# --------------------------------------------------------------------------- #
# active_claims=None == NOT MEASURED. Same sentinel discipline as
# node_pressure_score: `light` capacity mode and a failed SDK list both leave
# the field unmeasured, and 0 is the most attractive value LeastLoaded can
# see, so an unmeasured cluster must not win the tiebreak.
# --------------------------------------------------------------------------- #

def test_least_loaded_does_not_prefer_unmeasured_over_measured():
    reg = _reg(
        PlannerCluster(name="broken", active_claims=None, report_age_s=1),
        PlannerCluster(name="busy", active_claims=500, report_age_s=1),
    )
    # Treating None as 0 would pick "broken" — the cluster we know least about.
    assert LeastLoaded().select("img", reg).name == "busy"


def test_least_loaded_degrades_to_active_replicas_when_none_measured():
    # What --capacity-detail=light advertises: nothing measured anywhere, so
    # the first key ties across the board and active_replicas decides.
    reg = _reg(
        PlannerCluster(name="a", active_claims=None, warmpool_depth=9,
                       report_age_s=1),
        PlannerCluster(name="b", active_claims=None, warmpool_depth=1,
                       report_age_s=1),
    )
    assert LeastLoaded().select("img", reg).name == "b"


def test_least_loaded_still_ranks_measured_clusters_normally():
    reg = _reg(
        PlannerCluster(name="a", active_claims=10, report_age_s=1),
        PlannerCluster(name="b", active_claims=2, report_age_s=1),
        PlannerCluster(name="c", active_claims=None, report_age_s=1),
    )
    assert LeastLoaded().select("img", reg).name == "b"


# --------------------------------------------------------------------------- #
# PlannerRegistry.__iter__ must be annotated Iterator, not Iterable.
#
# Runtime behavior was always correct -- iter() returns a list_iterator either
# way. The bug is in the type: the Iterable protocol is *defined* as "has
# __iter__ returning an Iterator", so the weaker annotation meant
# PlannerRegistry did not formally satisfy Iterable[PlannerCluster] and any
# caller typed against it failed to check. mypy is the real guard; this pins
# the annotation so a future edit cannot quietly widen it back.
# --------------------------------------------------------------------------- #

def test_planner_registry_iter_is_annotated_as_an_iterator():
    import typing

    hints = typing.get_type_hints(PlannerRegistry.__iter__)
    assert hints["return"] == typing.Iterator[PlannerCluster], (
        f"__iter__ returns {hints['return']}; the Iterable protocol requires "
        f"an Iterator"
    )


def test_planner_registry_satisfies_the_iterable_protocol_at_runtime():
    import collections.abc

    reg = _reg(PlannerCluster(name="a"), PlannerCluster(name="b"))
    assert isinstance(reg, collections.abc.Iterable)
    assert isinstance(iter(reg), collections.abc.Iterator)
    assert sorted(c.name for c in reg) == ["a", "b"]


# --------------------------------------------------------------------------- #
# fresh() vs eligible(): reachability vs "may receive work".
# --------------------------------------------------------------------------- #

def _mixed_registry():
    from agent_sandbox_fleet.placement import PlannerCluster, PlannerRegistry
    reg = PlannerRegistry()
    reg.clusters["live"] = PlannerCluster(name="live", weight=1.0, report_age_s=1)
    reg.clusters["drained"] = PlannerCluster(name="drained", weight=0.0, report_age_s=1)
    reg.clusters["stale"] = PlannerCluster(name="stale", weight=1.0, report_age_s=1e9)
    return reg


def test_fresh_answers_reachability_and_keeps_drained_clusters():
    # A drained cluster is still talking to us, and `fleetctl status` has to be
    # able to say so. Collapsing the two questions into one predicate would
    # make a drain indistinguishable from a cluster that fell off the network.
    reg = _mixed_registry()
    assert sorted(c.name for c in reg.fresh()) == ["drained", "live"]


def test_eligible_answers_may_receive_work_and_drops_both():
    reg = _mixed_registry()
    assert [c.name for c in reg.eligible()] == ["live"]


def test_iterating_a_registry_yields_eligible_not_fresh():
    # Every selector is written as "score whatever the registry hands me", so
    # the drain has to be enforced at __iter__ or each selector has to remember
    # to apply it -- and two placement paths do not go through a selector at all.
    reg = _mixed_registry()
    assert [c.name for c in reg] == ["live"]
    assert reg.names() == ["live"]


def test_no_selector_returns_a_drained_cluster():
    from agent_sandbox_fleet.placement import _REGISTRY, get_placement
    reg = _mixed_registry()
    for name in sorted(_REGISTRY):
        chosen = get_placement(name).select("gcr.io/x/img:v1", _mixed_registry())
        assert chosen.name == "live", f"{name} selected {chosen.name}"
    assert [c.name for c in reg.eligible()] == ["live"]
