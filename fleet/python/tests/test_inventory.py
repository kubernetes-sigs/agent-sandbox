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

"""Inventory provider tests — GCS and ClusterProfile.

The load-bearing test here is `test_gcs_and_clusterprofile_plan_identically`:
the two providers must be interchangeable from the planner's point of view, or
the whole "swap the inventory, leave the algorithm alone" claim is wrong.
"""

from __future__ import annotations

import datetime as _dt
import json
import logging
from dataclasses import asdict

import pytest

from agent_sandbox_fleet import inventory
from agent_sandbox_fleet.inventory import (
    ClusterProfileInventory,
    GCSInventory,
    age_seconds,
    get_inventory,
)
from agent_sandbox_fleet.planner import FleetSpec, ModelSpec, plan


# --------------------------------------------------------------------------- #
# Fakes
# --------------------------------------------------------------------------- #

def _now_iso(offset_s: float = 0.0) -> str:
    t = _dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(seconds=offset_s)
    return t.isoformat().replace("+00:00", "Z")


class FakeCustomObjectsApi:
    """Stands in for kubernetes.client.CustomObjectsApi."""

    def __init__(self, items):
        self.items = items
        self.calls = []

    def list_namespaced_custom_object(self, **kwargs):
        self.calls.append(kwargs)
        return {"items": self.items}


class FakeGCS:
    """Stands in for objectstore.GCS — only the surface the provider uses."""

    def __init__(self, reports: dict[str, dict], bucket: str = "fake-bucket"):
        # reports: cluster name -> report body
        self._blobs = {
            f"fleet/capacity/{name}.json": body for name, body in reports.items()
        }
        self._bucket = bucket

    @property
    def bucket_name(self) -> str:
        # Mirrors objectstore.GCS.bucket_name. Not used for lookups — the provider
        # reads it to name its source in the log line that says which inventory a
        # registry came from.
        return self._bucket

    def list_prefix(self, prefix):
        return [k for k in self._blobs if k.startswith(prefix)]

    def get_json(self, path):
        return self._blobs.get(path)


def _profile(name, *, properties=None, conditions=None, labels=None):
    meta = {"name": name, "namespace": "fleet-system"}
    if labels:
        meta["labels"] = labels
    return {
        "apiVersion": "multicluster.x-k8s.io/v1alpha1",
        "kind": "ClusterProfile",
        "metadata": meta,
        "spec": {"displayName": name, "clusterManager": {"name": "gke-fleet"}},
        "status": {
            "version": {"kubernetes": "1.31.0"},
            "properties": [{"name": k, "value": str(v)}
                           for k, v in (properties or {}).items()],
            "conditions": conditions if conditions is not None else [
                {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "True",
                 "lastTransitionTime": _now_iso(3600)},
                {"type": inventory.COND_JOINED, "status": "True",
                 "lastTransitionTime": _now_iso(3600)},
            ],
        },
    }


def _healthy_profile(name, **props):
    base = {
        inventory.PROP_HEARTBEAT: _now_iso(5),
        inventory.PROP_WARMPOOL_DEPTH: 1000,
        inventory.PROP_WARMPOOL_READY: 900,
        inventory.PROP_ACTIVE_CLAIMS: 42,
        inventory.PROP_CLAIM_P90_MS: 530.0,
        inventory.PROP_NODE_PRESSURE: 0.25,
    }
    base.update(props)
    return _profile(name, properties=base)


# --------------------------------------------------------------------------- #
# Property mapping
# --------------------------------------------------------------------------- #

def test_properties_map_onto_planner_cluster():
    api = FakeCustomObjectsApi([_healthy_profile("a")])
    reg = ClusterProfileInventory(api=api).load({})

    c = reg.clusters["a"]
    assert c.name == "a"
    assert c.warmpool_depth == 1000
    assert c.warmpool_ready == 900
    assert c.active_claims == 42
    assert c.claim_p90_ms == 530.0
    assert c.node_pressure_score == 0.25
    assert c.report_age_s < 90
    assert reg.fresh() == [c]


def test_absent_pressure_stays_none_not_zero():
    # 0.0 reads as "idle" and CapacityAware would then *prefer* the cluster that
    # failed to measure itself. Same invariant the GCS provider holds.
    profile = _healthy_profile("a")
    profile["status"]["properties"] = [
        p for p in profile["status"]["properties"]
        if p["name"] != inventory.PROP_NODE_PRESSURE
    ]
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].node_pressure_score is None


def test_malformed_property_is_ignored_not_fatal():
    profile = _healthy_profile("a", **{inventory.PROP_WARMPOOL_DEPTH: "not-a-number"})
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].warmpool_depth == 0


def test_max_replicas_populated_only_when_published():
    plain = ClusterProfileInventory(
        api=FakeCustomObjectsApi([_healthy_profile("a")])).load({})
    assert plain.clusters["a"].max_replicas is None

    capped = ClusterProfileInventory(api=FakeCustomObjectsApi([
        _healthy_profile("a", **{inventory.PROP_MAX_REPLICAS: 150000})])).load({})
    assert capped.clusters["a"].max_replicas == 150000


def test_profile_without_name_is_skipped():
    bad = _healthy_profile("a")
    bad["metadata"] = {}
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([bad])).load({})
    assert reg.clusters == {}


# --------------------------------------------------------------------------- #
# Freshness — the heartbeat gap
# --------------------------------------------------------------------------- #

def test_unhealthy_control_plane_is_excluded():
    profile = _healthy_profile("a")
    profile["status"]["conditions"] = [
        {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "False",
         "lastTransitionTime": _now_iso(60)},
    ]
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].report_age_s == inventory.STALE_AGE_S
    assert reg.fresh() == []


def test_unjoined_cluster_is_excluded():
    profile = _healthy_profile("a")
    profile["status"]["conditions"] = [
        {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "True",
         "lastTransitionTime": _now_iso(60)},
        {"type": inventory.COND_JOINED, "status": "False",
         "lastTransitionTime": _now_iso(60)},
    ]
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.fresh() == []


def test_stale_heartbeat_is_excluded():
    profile = _healthy_profile("a", **{inventory.PROP_HEARTBEAT: _now_iso(300)})
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].report_age_s > 90
    assert reg.fresh() == []


def test_missing_heartbeat_falls_back_to_condition_by_default():
    # The documented gap: ControlPlaneHealthy=True with an hour-old
    # lastTransitionTime still reads as live, because a condition's timestamp
    # does not refresh. Default behavior trusts it.
    profile = _healthy_profile("a")
    profile["status"]["properties"] = [
        p for p in profile["status"]["properties"]
        if p["name"] != inventory.PROP_HEARTBEAT
    ]
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].report_age_s == 0.0
    assert len(reg.fresh()) == 1


def test_condition_fallback_is_distinguishable_from_a_real_zero_age():
    # The fallback synthesizes report_age_s=0.0, which makes the cluster we know
    # LEAST about look like the freshest one in the fleet. Freshness is only
    # threshold-tested today, so this is currently cosmetic — but any consumer
    # that ranks on age needs to be able to tell the two apart.
    without = _healthy_profile("a")
    without["status"]["properties"] = [
        p for p in without["status"]["properties"]
        if p["name"] != inventory.PROP_HEARTBEAT
    ]
    prov = ClusterProfileInventory(
        api=FakeCustomObjectsApi([without, _healthy_profile("b")]))
    reg = prov.load({})

    assert reg.clusters["a"].report_age_s == 0.0
    assert prov.clusters_without_heartbeat == frozenset({"a"})
    # b measured a real age and must not be tarred with the same brush.
    assert "b" not in prov.clusters_without_heartbeat
    assert reg.clusters["b"].report_age_s > 0


def test_require_heartbeat_excludes_a_profile_without_one():
    profile = _healthy_profile("a")
    profile["status"]["properties"] = [
        p for p in profile["status"]["properties"]
        if p["name"] != inventory.PROP_HEARTBEAT
    ]
    reg = ClusterProfileInventory(
        api=FakeCustomObjectsApi([profile]), require_heartbeat=True).load({})
    assert reg.clusters["a"].report_age_s == inventory.STALE_AGE_S
    assert reg.fresh() == []


def test_no_heartbeat_and_no_condition_is_excluded():
    profile = _profile("a", properties={inventory.PROP_WARMPOOL_DEPTH: 5},
                       conditions=[])
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].report_age_s == inventory.STALE_AGE_S


# --------------------------------------------------------------------------- #
# Weights
# --------------------------------------------------------------------------- #

def test_operator_weight_overrides_derived_capacity():
    profile = _healthy_profile("a", **{inventory.PROP_SANDBOX_CAPACITY: 199000})
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({"a": 7.5})
    assert reg.clusters["a"].weight == 7.5


def test_weight_derives_from_published_capacity():
    # The point of the provider: no human multiplying node counts by hand.
    profile = _healthy_profile("a", **{inventory.PROP_SANDBOX_CAPACITY: 199000})
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].weight == 199000.0


def test_weight_defaults_to_one_without_capacity():
    reg = ClusterProfileInventory(
        api=FakeCustomObjectsApi([_healthy_profile("a")])).load({})
    assert reg.clusters["a"].weight == 1.0


def test_a_healthy_cluster_publishing_no_capacity_warns(caplog):
    # The 1.0 fallback above is the quietest way to lose a cluster: it stays
    # Joined, healthy, heartbeating and placement-eligible, so nothing errors --
    # hamilton_split just hands it a rounding error. On the 1M spec that put 500
    # sandboxes on a cluster sized for 129,000 and left the fleet 12% short with
    # a full-looking plan. Assert it is at least loud.
    with caplog.at_level(logging.WARNING, logger="agent_sandbox_fleet.inventory"):
        reg = ClusterProfileInventory(
            api=FakeCustomObjectsApi([_healthy_profile("a")])).load({})
    assert reg.clusters["a"].weight == 1.0
    assert inventory.PROP_SANDBOX_CAPACITY in caplog.text
    assert "a" in caplog.text


def test_no_capacity_warning_when_the_operator_pinned_the_weight(caplog):
    # Pinned weights are a legitimate choice, and the capacity property is then
    # irrelevant -- warning there would train the operator to ignore the warning
    # that matters.
    with caplog.at_level(logging.WARNING, logger="agent_sandbox_fleet.inventory"):
        reg = ClusterProfileInventory(
            api=FakeCustomObjectsApi([_healthy_profile("a")])).load({"a": 7.5})
    assert reg.clusters["a"].weight == 7.5
    assert inventory.PROP_SANDBOX_CAPACITY not in caplog.text


# --------------------------------------------------------------------------- #
# Shared behavior across providers
# --------------------------------------------------------------------------- #

def test_named_but_missing_cluster_gets_stale_placeholder():
    # Both providers must surface a silent cluster rather than hide it, so it
    # still shows up in `fleetctl status` as STALE.
    cp = ClusterProfileInventory(
        api=FakeCustomObjectsApi([_healthy_profile("a")])).load({"a": 1.0, "b": 2.0})
    assert cp.clusters["b"].report_age_s == inventory.STALE_AGE_S
    assert cp.clusters["b"].weight == 2.0
    assert [c.name for c in cp.fresh()] == ["a"]

    gcs = GCSInventory(FakeGCS({"a": {
        "cluster": "a", "updated_at": _now_iso(5), "warmpool_depth": 10,
    }})).load({"a": 1.0, "b": 2.0})
    assert gcs.clusters["b"].report_age_s == inventory.STALE_AGE_S
    assert [c.name for c in gcs.fresh()] == ["a"]


def test_each_provider_logs_which_inventory_it_read(caplog):
    # A healthy cycle used to log a cluster COUNT and nothing else, so a planner
    # reading ClusterProfiles and a planner silently still reading GCS produced
    # byte-identical output. Confirming which one was live cost a falsification
    # run against a live fleet. The source belongs in the log.
    with caplog.at_level(logging.INFO, logger="agent_sandbox_fleet.inventory"):
        GCSInventory(FakeGCS({"a": {"cluster": "a", "updated_at": _now_iso(5)}},
                             bucket="my-bucket")).load({})
    assert "gcs://my-bucket/" in caplog.text
    assert "clusterprofile" not in caplog.text

    caplog.clear()
    with caplog.at_level(logging.INFO, logger="agent_sandbox_fleet.inventory"):
        ClusterProfileInventory(
            api=FakeCustomObjectsApi([_healthy_profile("a")]),
            cluster_manager="gke-fleet", namespace="fleet-system").load({})
    assert "clusterprofile" in caplog.text
    assert f"{inventory.CLUSTER_MANAGER_LABEL}=gke-fleet" in caplog.text
    assert "gcs://" not in caplog.text


def test_the_log_distinguishes_a_real_cluster_from_a_stale_placeholder(caplog):
    # Both are "1 cluster". Only one of them is a cluster that actually reported,
    # and reading `--cluster-manager` wrong yields exactly the placeholder case.
    with caplog.at_level(logging.INFO, logger="agent_sandbox_fleet.inventory"):
        ClusterProfileInventory(
            api=FakeCustomObjectsApi([]), cluster_manager="nobody").load({"a": 1.0})
    assert "1 clusters, 0 placement-eligible" in caplog.text
    assert "NO REPORT" in caplog.text


def test_a_stale_heartbeat_is_not_reported_as_eligible(caplog):
    # REGRESSION. The first version of this log line compared against
    # STALE_AGE_S, which is a 1e9 sentinel rather than the freshness threshold,
    # so a cluster with a 10-minute-old heartbeat logged as "fresh" in the very
    # cycle the planner dropped it from placement. Observed live on 2026-08-12:
    # "2 clusters, 2 fresh — cp-e2e-b(age=623s)" alongside "pools": [] for that
    # same cluster. The log must agree with placement.fresh().
    profile = _profile("a", properties={
        inventory.PROP_HEARTBEAT: _now_iso(600),   # 10 minutes old
        inventory.PROP_WARMPOOL_DEPTH: 1000,
        inventory.PROP_WARMPOOL_READY: 900,
        inventory.PROP_ACTIVE_CLAIMS: 42,
        inventory.PROP_CLAIM_P90_MS: 530.0,
        inventory.PROP_NODE_PRESSURE: 0.25,
    })

    with caplog.at_level(logging.INFO, logger="agent_sandbox_fleet.inventory"):
        reg = ClusterProfileInventory(
            api=FakeCustomObjectsApi([profile]), require_heartbeat=True).load({})

    assert "0 placement-eligible" in caplog.text
    assert "STALE" in caplog.text
    assert reg.fresh() == [], "log and placement.fresh() must agree"


def test_the_log_line_is_bounded_and_always_samples_eligible_clusters(caplog):
    # REGRESSION, from the 2026-08-13 inventory scale run. The line enumerated
    # every cluster unconditionally: 507 clusters on a real hub produced a ~15KB
    # single line every 60s loop. Truncating alphabetically would have been worse
    # than not truncating -- there the first dozen names were stale placeholders
    # and synthetic scale-* entries, and the only placement-eligible cluster in
    # the fleet sorted past the cut. So the bound has to sample eligible
    # clusters first.
    profiles = [_profile(f"scale-{i:03d}", properties={
        inventory.PROP_HEARTBEAT: _now_iso(600),
        inventory.PROP_WARMPOOL_DEPTH: 1000,
    }) for i in range(1, 501)]
    profiles.append(_healthy_profile("zz-the-only-live-one"))

    with caplog.at_level(logging.INFO, logger="agent_sandbox_fleet.inventory"):
        ClusterProfileInventory(api=FakeCustomObjectsApi(profiles),
                                require_heartbeat=True).load({})

    line = next(ln for ln in caplog.text.splitlines() if "inventory " in ln)
    assert "501 clusters, 1 placement-eligible" in line
    # Sorts last of 501, and is the entire point of the line.
    assert "zz-the-only-live-one(age=" in line
    assert "…+489 more" in line
    assert "500 stale, 0 no-report" in line
    assert len(line) < 1024, f"log line is {len(line)} bytes"


def test_cluster_manager_label_selector_is_applied():
    api = FakeCustomObjectsApi([_healthy_profile("a")])
    ClusterProfileInventory(api=api, cluster_manager="gke-fleet").load({})
    assert api.calls[0]["label_selector"] == (
        f"{inventory.CLUSTER_MANAGER_LABEL}=gke-fleet")

    api2 = FakeCustomObjectsApi([_healthy_profile("a")])
    ClusterProfileInventory(api=api2).load({})
    assert "label_selector" not in api2.calls[0]


def test_namespace_and_group_coordinates():
    api = FakeCustomObjectsApi([])
    ClusterProfileInventory(api=api, namespace="my-fleet").load({})
    call = api.calls[0]
    assert call["group"] == "multicluster.x-k8s.io"
    assert call["version"] == "v1alpha1"
    assert call["plural"] == "clusterprofiles"
    assert call["namespace"] == "my-fleet"


# --------------------------------------------------------------------------- #
# The load-bearing test: providers are interchangeable.
# --------------------------------------------------------------------------- #

def test_gcs_and_clusterprofile_plan_identically():
    """Same cluster state via either provider must produce the same assignment.

    This is what makes the ClusterProfile work a swap of the inventory layer
    rather than a change to placement.
    """
    state = {
        "a": dict(depth=1000, ready=900, claims=42, p90=530.0, pressure=0.25),
        "b": dict(depth=500, ready=500, claims=10, p90=210.0, pressure=0.10),
        "c": dict(depth=0, ready=0, claims=0, p90=0.0, pressure=0.05),
    }
    ts = _now_iso(5)
    weights = {"a": 1.0, "b": 2.0, "c": 3.0}

    gcs_reg = GCSInventory(FakeGCS({
        name: {
            "cluster": name,
            "updated_at": ts,
            "warmpool_depth": s["depth"],
            "warmpool_ready": s["ready"],
            "active_claims": s["claims"],
            "claim_p90_ms": s["p90"],
            "node_pressure_score": s["pressure"],
        } for name, s in state.items()
    })).load(weights)

    cp_reg = ClusterProfileInventory(api=FakeCustomObjectsApi([
        _profile(name, properties={
            inventory.PROP_HEARTBEAT: ts,
            inventory.PROP_WARMPOOL_DEPTH: s["depth"],
            inventory.PROP_WARMPOOL_READY: s["ready"],
            inventory.PROP_ACTIVE_CLAIMS: s["claims"],
            inventory.PROP_CLAIM_P90_MS: s["p90"],
            inventory.PROP_NODE_PRESSURE: s["pressure"],
        }) for name, s in state.items()
    ])).load(weights)

    # The registries must agree field-for-field, not merely produce the same
    # plan by luck. report_age_s is a measured elapsed time, so it is compared
    # as "both fresh" rather than for equality.
    def _comparable(reg):
        out = {}
        for name, c in reg.clusters.items():
            d = asdict(c)
            assert d.pop("report_age_s") < 90
            out[name] = d
        return out

    assert _comparable(gcs_reg) == _comparable(cp_reg)

    spec = FleetSpec(
        max_concurrent=600, max_pool=200,
        placement_policy="capacity-aware",
        cluster_weights={"a": 1.0, "b": 2.0, "c": 3.0},
        models=[ModelSpec(image=f"img-{i}", template_name=f"tmpl-{i}",
                          target_tasks=100) for i in range(6)],
    )

    from_gcs = plan(spec, gcs_reg)
    from_cp = plan(spec, cp_reg)

    # updated_at is a wall-clock stamp; everything else must match exactly.
    assert from_gcs.model_dump()["clusters"] == from_cp.model_dump()["clusters"]


def test_parity_test_can_actually_fail():
    """Negative control for the test above.

    A parity assertion is worthless if the plan is insensitive to the inventory
    data feeding it. Perturb one cluster's load on one side and the plans must
    diverge — otherwise the parity test proves nothing.

    Note which signal is perturbed: CapacityAware scores on warmpool_depth and
    warmpool_ready (via active_replicas / ready_ratio), NOT on active_claims —
    that one only breaks ties inside the LeastLoaded fallback.
    """
    ts = _now_iso(5)
    weights = {"a": 1.0, "b": 1.0}

    def registry(b_depth, b_ready):
        return ClusterProfileInventory(api=FakeCustomObjectsApi([
            _profile("a", properties={inventory.PROP_HEARTBEAT: ts,
                                      inventory.PROP_WARMPOOL_DEPTH: 100,
                                      inventory.PROP_WARMPOOL_READY: 100}),
            _profile("b", properties={inventory.PROP_HEARTBEAT: ts,
                                      inventory.PROP_WARMPOOL_DEPTH: b_depth,
                                      inventory.PROP_WARMPOOL_READY: b_ready}),
        ])).load(weights)

    spec = FleetSpec(
        max_concurrent=600, max_pool=200,
        placement_policy="capacity-aware",
        cluster_weights=weights,
        models=[ModelSpec(image=f"img-{i}", template_name=f"tmpl-{i}",
                          target_tasks=100) for i in range(4)],
    )

    balanced = plan(spec, registry(100, 100)).model_dump()["clusters"]
    b_saturated = plan(spec, registry(10000, 100)).model_dump()["clusters"]
    assert balanced != b_saturated
    # ...and specifically: a saturated b must stop attracting the extra pools.
    assert len(b_saturated["a"]["pools"]) > len(balanced["a"]["pools"])


# --------------------------------------------------------------------------- #
# Factory + helpers
# --------------------------------------------------------------------------- #

def test_get_inventory_dispatch():
    assert isinstance(get_inventory("gcs", gcs=FakeGCS({})), GCSInventory)
    assert isinstance(get_inventory("clusterprofile"), ClusterProfileInventory)


def test_get_inventory_rejects_unknown_and_missing_gcs():
    import pytest
    with pytest.raises(ValueError, match="unknown inventory"):
        get_inventory("etcd")
    with pytest.raises(ValueError, match="needs a GCS client"):
        get_inventory("gcs")


# --------------------------------------------------------------------------- #
# CLI wiring
# --------------------------------------------------------------------------- #

def test_cli_defaults_to_gcs_and_honours_the_flag(monkeypatch):
    from agent_sandbox_fleet import cli

    monkeypatch.delenv("FLEET_INVENTORY", raising=False)
    parser = cli.build_parser()
    gcs = FakeGCS({})

    args = parser.parse_args(["apply", "-f", "spec.yaml"])
    assert isinstance(cli._provider_from(args, gcs), GCSInventory)

    args = parser.parse_args(
        ["apply", "-f", "spec.yaml", "--inventory", "clusterprofile",
         "--hub-namespace", "my-fleet", "--cluster-manager", "gke-fleet",
         "--require-heartbeat"])
    prov = cli._provider_from(args, gcs)
    assert isinstance(prov, ClusterProfileInventory)
    assert prov._namespace == "my-fleet"
    assert prov._cluster_manager == "gke-fleet"
    assert prov._require_heartbeat is True


def test_cli_reads_fleet_inventory_env(monkeypatch):
    from agent_sandbox_fleet import cli

    monkeypatch.setenv("FLEET_INVENTORY", "clusterprofile")
    args = cli.build_parser().parse_args(["apply", "-f", "spec.yaml"])
    assert isinstance(cli._provider_from(args, FakeGCS({})), ClusterProfileInventory)


def test_show_registry_needs_no_bucket_on_the_clusterprofile_path(monkeypatch, capsys):
    # "Consume ClusterProfiles" should mean the hub is sufficient. Weights come
    # from each cluster's own sandbox-capacity when there is no spec to override
    # them.
    import pytest
    from agent_sandbox_fleet import cli

    monkeypatch.delenv("FLEET_BUCKET", raising=False)
    monkeypatch.delenv("FLEET_INVENTORY", raising=False)
    monkeypatch.setattr(
        cli, "_provider_from",
        lambda args, gcs, paths=None: ClusterProfileInventory(
            api=FakeCustomObjectsApi([
                _healthy_profile("a", **{inventory.PROP_SANDBOX_CAPACITY: 199000}),
            ])))

    args = cli.build_parser().parse_args(
        ["show-registry", "--inventory", "clusterprofile"])
    assert cli.cmd_show_registry(args) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["a"]["weight"] == 199000.0
    assert out["a"]["fresh"] is True

    # ...but the GCS path still requires one, rather than silently reading empty.
    args = cli.build_parser().parse_args(["show-registry"])
    with pytest.raises(SystemExit, match="FLEET_BUCKET"):
        cli.cmd_show_registry(args)


def test_age_seconds_handles_garbage_and_naive_timestamps():
    assert age_seconds(None) == inventory.STALE_AGE_S
    assert age_seconds("") == inventory.STALE_AGE_S
    assert age_seconds("yesterday") == inventory.STALE_AGE_S
    assert age_seconds(_now_iso(10)) >= 9
    # A naive timestamp is assumed UTC rather than raising.
    naive = (_dt.datetime.now(_dt.timezone.utc)
             .replace(tzinfo=None) - _dt.timedelta(seconds=30)).isoformat()
    assert 25 <= age_seconds(naive) <= 35


# --------------------------------------------------------------------------- #
# Untrusted ClusterProfile properties.
#
# Properties are free-form strings written by whatever publishes the profile.
# float() accepts "inf" and "nan": inf wins every least-loaded comparison, nan
# loses every one of them, and int(float("inf")) raises OverflowError rather
# than the ValueError the obvious `except` catches — so one malformed profile
# could take down the whole plan.
# --------------------------------------------------------------------------- #

@pytest.mark.parametrize("bad", ["inf", "-inf", "Infinity", "nan", "NaN"])
def test_non_finite_int_property_is_ignored_not_fatal(bad):
    api = FakeCustomObjectsApi([
        _healthy_profile("a", **{inventory.PROP_WARMPOOL_DEPTH: bad})])
    reg = ClusterProfileInventory(api=api).load({})
    # Degrades to the "unset" default; must not raise.
    assert reg.clusters["a"].warmpool_depth == 0


@pytest.mark.parametrize("bad", ["inf", "-inf", "nan"])
def test_non_finite_float_property_is_ignored_not_fatal(bad):
    api = FakeCustomObjectsApi([
        _healthy_profile("a", **{inventory.PROP_NODE_PRESSURE: bad})])
    reg = ClusterProfileInventory(api=api).load({})
    # None, not inf/nan: unmeasured, so CapacityAware stays pressure-blind
    # rather than ranking on a poisoned score.
    assert reg.clusters["a"].node_pressure_score is None


def test_garbage_property_is_still_ignored():
    api = FakeCustomObjectsApi([
        _healthy_profile("a", **{inventory.PROP_ACTIVE_CLAIMS: "lots"})])
    reg = ClusterProfileInventory(api=api).load({})
    assert reg.clusters["a"].active_claims is None


def test_absent_claims_property_reads_as_unmeasured_not_zero():
    # A member running --capacity-detail=light omits the property entirely.
    profile = _healthy_profile("a")
    profile["status"]["properties"] = [
        p for p in profile["status"]["properties"]
        if p["name"] != inventory.PROP_ACTIVE_CLAIMS
    ]
    reg = ClusterProfileInventory(api=FakeCustomObjectsApi([profile])).load({})
    assert reg.clusters["a"].active_claims is None


def test_gcs_report_with_null_claims_reads_as_unmeasured():
    gcs = FakeGCS({"a": {
        "cluster": "a",
        "updated_at": _now_iso(5),
        "warmpool_depth": 10,
        "warmpool_ready": 10,
        "active_claims": None,
        "node_pressure_score": None,
    }})
    reg = GCSInventory(gcs).load({})
    assert reg.clusters["a"].active_claims is None
    assert reg.clusters["a"].node_pressure_score is None


@pytest.mark.parametrize("bad", ["0", "-1", "-0.5", "nan", "inf", "abc"])
def test_apply_rejects_a_non_positive_loop_interval(bad):
    # The loop sleeps max(0.0, interval - elapsed), so a non-positive interval
    # spins: it re-plans and rewrites assignments.json as fast as GCS answers.
    from agent_sandbox_fleet import cli

    with pytest.raises(SystemExit):
        cli.build_parser().parse_args(
            ["apply", "-f", "spec.yaml", "--loop", "--loop-interval", bad])


def test_apply_accepts_a_positive_loop_interval():
    from agent_sandbox_fleet import cli

    args = cli.build_parser().parse_args(
        ["apply", "-f", "spec.yaml", "--loop", "--loop-interval", "0.5"])
    assert args.loop_interval == 0.5


# --------------------------------------------------------------------------- #
# One bad capacity report must not take the planner down.
#
# These objects are written by N independently-deployed member pods. A rolling
# upgrade mid-write, a truncated object, an older member schema -- any one of
# them used to raise straight out of GCSInventory.load(), so `fleetctl apply`
# could place nothing anywhere. The blast radius of one sick cluster has to be
# that cluster.
# --------------------------------------------------------------------------- #

@pytest.mark.parametrize("bad,reason", [
    ({"warmpool_depth": 3}, "no cluster field"),
    ({"cluster": None}, "null cluster"),
    ({"cluster": ""}, "empty cluster"),
    ({"cluster": "b", "warmpool_depth": "many"}, "unparseable int"),
    ({"cluster": "b", "node_pressure_score": "high"}, "unparseable float"),
    ({"cluster": "b", "active_claims": []}, "wrong type"),
    (["not", "an", "object"], "JSON array, not an object"),
    ("garbage", "JSON string, not an object"),
])
def test_one_malformed_report_does_not_sink_the_registry(bad, reason, caplog):
    reg = GCSInventory(FakeGCS({
        "a": {"cluster": "a", "warmpool_depth": 5, "warmpool_ready": 5,
              "updated_at": _now_iso()},
        "b": bad,
    })).load({"a": 1.0, "b": 1.0})

    assert reg.clusters["a"].warmpool_depth == 5, f"healthy peer lost: {reason}"
    # b is still present -- _stale_placeholders puts it there -- but as a stale
    # placeholder, which is exactly "visible in show-registry, ineligible for
    # placement" rather than a half-populated cluster the planner would trust.
    assert "b" in reg.clusters
    assert reg.clusters["b"] not in reg.fresh()


def test_a_rejected_report_contributes_no_partial_state(caplog):
    # PlannerCluster is built in one expression, so a raise while evaluating any
    # field means the assignment into reg.clusters never ran. A later refactor to
    # field-by-field construction would break that, and the symptom would be a
    # cluster the planner treats as measured on the strength of whichever fields
    # happened to parse before the bad one. Pin the outcome.
    caplog.set_level(logging.WARNING)
    reg = GCSInventory(FakeGCS({
        "b": {"cluster": "b", "warmpool_depth": 4, "claim_p90_ms": "soon",
              "updated_at": _now_iso()},
    })).load({"b": 1.0})

    assert reg.clusters["b"].warmpool_depth == 0, (
        "a field from the rejected report survived into the placeholder"
    )
    assert reg.clusters["b"] not in reg.fresh()
    assert any("skipping capacity report" in r.message for r in caplog.records)
