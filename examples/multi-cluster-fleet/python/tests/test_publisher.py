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

"""Publisher tests — the write half of the ClusterProfile integration.

The load-bearing test is `test_round_trip_through_the_inventory_provider`:
what the member writes must be exactly what the planner reads. Testing the two
halves separately would let them drift while both suites stayed green.
"""

from __future__ import annotations

import datetime as _dt

import pytest

from agent_sandbox_fleet import inventory
from agent_sandbox_fleet.inventory import ClusterProfileInventory
from agent_sandbox_fleet.publisher import (
    APPLY_CONTENT_TYPE,
    DEFAULT_FIELD_MANAGER,
    ClusterProfilePublisher,
)


def _now_iso(offset_s: float = 0.0) -> str:
    t = _dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(seconds=offset_s)
    return t.isoformat().replace("+00:00", "Z")


class FakeApplyApi:
    """Records applies. Mimics CustomObjectsApi's patch surface."""

    def __init__(self, content_type=APPLY_CONTENT_TYPE):
        self.status_calls = []
        self.object_calls = []

        class _Client:
            default_headers = {"Content-Type": content_type} if content_type else {}

        self.api_client = _Client()

    def patch_namespaced_custom_object_status(self, **kwargs):
        self.status_calls.append(kwargs)
        return kwargs["body"]

    def patch_namespaced_custom_object(self, **kwargs):
        self.object_calls.append(kwargs)
        return kwargs["body"]


def _report(**overrides):
    base = dict(
        cluster="cluster-a",
        updated_at=_now_iso(2),
        generation_observed=7,
        warmpool_depth=1000,
        warmpool_ready=900,
        active_claims=42,
        claim_p90_ms=530.0,
        node_pressure_score=0.25,
        reported_pools=["p1", "p2"],
    )
    base.update(overrides)
    return base


def _props(body):
    return {p["name"]: p["value"] for p in body["status"]["properties"]}


# --------------------------------------------------------------------------- #
# Body construction
# --------------------------------------------------------------------------- #

def test_body_carries_the_identity_ssa_requires():
    pub = ClusterProfilePublisher("cluster-a", api=FakeApplyApi())
    body = pub.build_body(_report())
    assert body["apiVersion"] == "multicluster.x-k8s.io/v1alpha1"
    assert body["kind"] == "ClusterProfile"
    assert body["metadata"]["name"] == "cluster-a"
    assert body["metadata"]["namespace"] == inventory.CLUSTERPROFILE_NAMESPACE


def test_capacity_maps_onto_properties():
    pub = ClusterProfilePublisher("cluster-a", api=FakeApplyApi())
    p = _props(pub.build_body(_report()))
    assert p[inventory.PROP_WARMPOOL_DEPTH] == "1000"
    assert p[inventory.PROP_WARMPOOL_READY] == "900"
    assert p[inventory.PROP_ACTIVE_CLAIMS] == "42"
    assert p[inventory.PROP_CLAIM_P90_MS] == "530.0"
    assert p[inventory.PROP_NODE_PRESSURE] == "0.25"
    assert p[inventory.PROP_HEARTBEAT]


def test_unmeasured_pressure_is_omitted_not_zeroed():
    # Publishing 0.0 would read as "idle" and CapacityAware would PREFER the
    # cluster whose pressure calc blew up. Observed on a density fleet at 200k
    # pods, where the calc failed every cycle. Omission keeps it None on read.
    pub = ClusterProfilePublisher("cluster-a", api=FakeApplyApi())
    p = _props(pub.build_body(_report(node_pressure_score=None)))
    assert inventory.PROP_NODE_PRESSURE not in p


def test_unmeasured_claims_are_omitted_not_zeroed():
    # Exactly the pressure rule above, applied to active_claims. 0 in-flight
    # claims is the most attractive value LeastLoaded can see, so a member in
    # `light` mode (or one whose SDK list failed) must not publish it.
    pub = ClusterProfilePublisher("cluster-a", api=FakeApplyApi())
    p = _props(pub.build_body(_report(active_claims=None)))
    assert inventory.PROP_ACTIVE_CLAIMS not in p


def test_capacity_and_max_replicas_are_published_when_configured():
    bare = ClusterProfilePublisher("a", api=FakeApplyApi())
    assert inventory.PROP_SANDBOX_CAPACITY not in _props(bare.build_body(_report()))

    sized = ClusterProfilePublisher(
        "a", api=FakeApplyApi(), sandbox_capacity=199000, max_replicas=210000)
    p = _props(sized.build_body(_report()))
    assert p[inventory.PROP_SANDBOX_CAPACITY] == "199000"
    assert p[inventory.PROP_MAX_REPLICAS] == "210000"


def test_conditions_are_never_written():
    # ControlPlaneHealthy / Joined belong to the cluster manager. A member
    # asserting its own control plane is healthy proves nothing anyway.
    pub = ClusterProfilePublisher("a", api=FakeApplyApi())
    assert "conditions" not in pub.build_body(_report())["status"]


# --------------------------------------------------------------------------- #
# The apply itself
# --------------------------------------------------------------------------- #

def test_apply_uses_the_status_subresource_and_our_field_manager():
    api = FakeApplyApi()
    ClusterProfilePublisher("cluster-a", api=api).publish(_report())

    assert len(api.status_calls) == 1 and not api.object_calls
    call = api.status_calls[0]
    assert call["field_manager"] == DEFAULT_FIELD_MANAGER
    assert call["plural"] == "clusterprofiles"
    assert call["name"] == "cluster-a"
    # Not forced by default: stealing another manager's field must be a choice.
    assert "force" not in call


def test_api_version_is_configurable_on_both_halves():
    # Upstream serves v1alpha1 (storage, deprecated) AND v1alpha2. Hardcoding
    # either one strands us: v1alpha1 disappears eventually, v1alpha2 is absent
    # on hubs running an older CRD.
    api = FakeApplyApi()
    pub = ClusterProfilePublisher("a", api=api, version="v1alpha2")
    body = pub.publish(_report())

    assert body["apiVersion"] == "multicluster.x-k8s.io/v1alpha2"
    assert api.status_calls[0]["version"] == "v1alpha2"

    read = ClusterProfileInventory(api=_VersionRecordingApi(), version="v1alpha2")
    read.list_profiles()
    assert read._api.seen == "v1alpha2"


def test_reader_and_writer_default_to_the_same_version():
    # A mismatch does not error — with no conversion webhook the apiserver
    # serves the same object under either version — it just splits ownership
    # into two managedFields entries under one manager name. Silent.
    api = FakeApplyApi()
    ClusterProfilePublisher("a", api=api).publish(_report())
    read = ClusterProfileInventory(api=_VersionRecordingApi())
    read.list_profiles()
    assert api.status_calls[0]["version"] == read._api.seen


class _VersionRecordingApi:
    seen = None

    def list_namespaced_custom_object(self, **kwargs):
        self.seen = kwargs["version"]
        return {"items": []}


def test_force_is_opt_in():
    api = FakeApplyApi()
    ClusterProfilePublisher("a", api=api, force=True).publish(_report())
    assert api.status_calls[0]["force"] is True


def test_non_subresource_mode_targets_the_main_object():
    api = FakeApplyApi()
    ClusterProfilePublisher("a", api=api, status_subresource=False).publish(_report())
    assert len(api.object_calls) == 1 and not api.status_calls


def test_ssa_guard_rejects_a_merge_patch_client():
    # The silent failure this exists to catch: merge-patch ACCEPTS field_manager
    # and simply establishes no ownership, so nothing errors until two managers
    # start overwriting each other.
    ClusterProfilePublisher("a", api=FakeApplyApi()).assert_ssa_configured()

    wrong = ClusterProfilePublisher(
        "a", api=FakeApplyApi(content_type="application/merge-patch+json"))
    with pytest.raises(RuntimeError, match="merge-patch"):
        wrong.assert_ssa_configured()


# --------------------------------------------------------------------------- #
# Round trip — the two halves must agree.
# --------------------------------------------------------------------------- #

class _ReadBackApi:
    """Serves whatever the publisher last applied, as a list response."""

    def __init__(self, applied):
        self._applied = applied

    def list_namespaced_custom_object(self, **_kwargs):
        return {"items": self._applied}


def test_round_trip_through_the_inventory_provider():
    """Write with the publisher, read with the provider, compare to the source.

    This is the contract between the two halves. Both use the same PROP_*
    constants, and this is what proves it — a rename on one side alone fails
    here rather than silently zeroing a field in production.
    """
    report = _report()
    pub = ClusterProfilePublisher(
        "cluster-a", api=FakeApplyApi(), sandbox_capacity=199000)
    applied = pub.publish(report)

    # The cluster manager's half: identity and health, which we never write.
    applied["status"]["conditions"] = [
        {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "True",
         "lastTransitionTime": _now_iso(3600)},
    ]

    reg = ClusterProfileInventory(api=_ReadBackApi([applied])).load({})
    c = reg.clusters["cluster-a"]

    assert c.warmpool_depth == report["warmpool_depth"]
    assert c.warmpool_ready == report["warmpool_ready"]
    assert c.active_claims == report["active_claims"]
    assert c.claim_p90_ms == report["claim_p90_ms"]
    assert c.node_pressure_score == report["node_pressure_score"]
    # Weight derived from the published capacity — no operator weight map.
    assert c.weight == 199000.0
    # Heartbeat survived the trip, so the cluster reads as live.
    assert c.report_age_s < 90
    assert reg.fresh() == [c]


def test_round_trip_preserves_unmeasured_pressure_as_none():
    # Omitted on write must arrive as None on read, NOT 0.0 — the invariant is
    # only worth anything if it survives the round trip.
    pub = ClusterProfilePublisher("a", api=FakeApplyApi())
    applied = pub.publish(_report(node_pressure_score=None))
    applied["status"]["conditions"] = [
        {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "True",
         "lastTransitionTime": _now_iso(60)},
    ]
    reg = ClusterProfileInventory(api=_ReadBackApi([applied])).load({})
    assert reg.clusters["a"].node_pressure_score is None


def test_round_trip_preserves_unmeasured_claims_as_none():
    pub = ClusterProfilePublisher("a", api=FakeApplyApi())
    applied = pub.publish(_report(active_claims=None))
    applied["status"]["conditions"] = [
        {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "True",
         "lastTransitionTime": _now_iso(60)},
    ]
    reg = ClusterProfileInventory(api=_ReadBackApi([applied])).load({})
    assert reg.clusters["a"].active_claims is None


# --------------------------------------------------------------------------- #
# The hub call has to be bounded.
#
# urllib3's default is no timeout at all. The hub is the one endpoint in this
# system that is routinely unreachable -- separate VPC, private endpoint, no
# --enable-master-global-access -- and an unreachable hub parked this thread
# until the OS gave up on the SYN, minutes at a time. That same thread also
# publishes to GCS, so the hub took down the path that does not depend on it.
# --------------------------------------------------------------------------- #

def test_publish_bounds_the_hub_call_by_default():
    api = FakeApplyApi()
    ClusterProfilePublisher("cluster-a", api=api).publish(_report())
    connect, read = api.status_calls[0]["_request_timeout"]
    assert connect > 0 and read > 0
    assert connect <= 10, "a connect timeout this long is not a timeout"


def test_the_request_timeout_is_configurable():
    api = FakeApplyApi()
    pub = ClusterProfilePublisher("cluster-a", api=api, request_timeout=3.0)
    pub.publish(_report())
    assert api.status_calls[0]["_request_timeout"] == 3.0


def test_the_timeout_can_be_opted_out_of():
    # None means "do not pass the kwarg", not "pass None" -- a client that does
    # not understand _request_timeout would choke on the latter.
    api = FakeApplyApi()
    pub = ClusterProfilePublisher("cluster-a", api=api, request_timeout=None)
    pub.publish(_report())
    assert "_request_timeout" not in api.status_calls[0]


def test_the_timeout_also_applies_to_the_non_subresource_path():
    api = FakeApplyApi()
    pub = ClusterProfilePublisher("cluster-a", api=api, status_subresource=False)
    pub.publish(_report())
    assert "_request_timeout" in api.object_calls[0]
