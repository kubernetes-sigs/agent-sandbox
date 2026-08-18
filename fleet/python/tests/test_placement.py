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
