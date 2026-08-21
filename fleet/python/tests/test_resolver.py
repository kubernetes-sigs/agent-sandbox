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

"""Tests for agent_sandbox_fleet.resolver."""

from __future__ import annotations

from typing import Any

import pytest

from agent_sandbox_fleet.resolver import (
    AssignmentsMissingError,
    ClusterResolver,
    ClusterUnavailableError,
    FleetSandboxClient,
    ResolverError,
    TemplateNotAssignedError,
    _looks_zonal,
    gke_context_naming,
    kind_context_naming,
)


# --------------------------------------------------------------------------- #
# Fake GCS — no google-cloud-storage dep in tests.
# --------------------------------------------------------------------------- #

class FakeGCS:
    """Minimal in-memory stand-in for objectstore.GCS.

    Only implements the methods the resolver actually calls (get_json). Tracks
    read count so tests can assert cache behavior.
    """

    def __init__(self, bucket: str = "test-bucket"):
        self.bucket_name = bucket
        self._data: dict[str, Any] = {}
        self.read_count = 0
        self.raise_next: Exception | None = None

    def set_assignments(self, data: dict[str, Any]) -> None:
        self._data["fleet/assignments.json"] = data

    def get_json(self, path: str) -> Any | None:
        if self.raise_next is not None:
            exc, self.raise_next = self.raise_next, None
            raise exc
        self.read_count += 1
        return self._data.get(path)


def _assn(clusters: dict[str, list[dict]], generation: int = 1) -> dict:
    """Build an assignments.json-shaped dict."""
    return {
        "generation": generation,
        "updated_at": "2026-07-16T00:00:00Z",
        "clusters": {
            c: {"pools": pools} for c, pools in clusters.items()
        },
    }


def _pool(template: str, replicas: int = 5, image: str | None = None) -> dict:
    return {
        "template": template,
        "warmpool": f"{template}-pool",
        "replicas": replicas,
        "image": image,
    }


# --------------------------------------------------------------------------- #
# resolve() basics
# --------------------------------------------------------------------------- #

def test_resolve_returns_hosting_cluster():
    gcs = FakeGCS()
    gcs.set_assignments(_assn({
        "cluster-a": [_pool("sb-tmpl-a")],
        "cluster-b": [_pool("sb-tmpl-b")],
    }))
    r = ClusterResolver("test", gcs=gcs, context_naming=lambda c: f"ctx-{c}")

    out = r.resolve("sb-tmpl-a")
    assert out.cluster == "cluster-a"
    assert out.template == "sb-tmpl-a"
    assert out.warmpool == "sb-tmpl-a-pool"
    assert out.replicas == 5
    assert out.context_name == "ctx-cluster-a"
    assert out.generation == 1


def test_resolve_raises_when_template_missing():
    gcs = FakeGCS()
    gcs.set_assignments(_assn({"cluster-a": [_pool("sb-tmpl-a")]}))
    r = ClusterResolver("test", gcs=gcs)
    with pytest.raises(TemplateNotAssignedError, match="sb-tmpl-missing"):
        r.resolve("sb-tmpl-missing")


def test_resolve_raises_when_assignments_missing():
    gcs = FakeGCS()  # empty
    r = ClusterResolver("test", gcs=gcs)
    with pytest.raises(AssignmentsMissingError):
        r.resolve("sb-tmpl-a")


def test_resolve_skips_pools_with_zero_replicas():
    """A cluster listed with a 0-replica pool for the template shouldn't count as
    a host — it means the fleet-member has drained the pool."""
    gcs = FakeGCS()
    gcs.set_assignments(_assn({
        "cluster-a": [_pool("sb-tmpl-a", replicas=0)],
        "cluster-b": [_pool("sb-tmpl-a", replicas=5)],
    }))
    r = ClusterResolver("test", gcs=gcs)
    out = r.resolve("sb-tmpl-a")
    assert out.cluster == "cluster-b"


# --------------------------------------------------------------------------- #
# Strategy: first / round-robin / hash
# --------------------------------------------------------------------------- #

def _three_cluster_gcs() -> FakeGCS:
    gcs = FakeGCS()
    gcs.set_assignments(_assn({
        "cluster-a": [_pool("shared")],
        "cluster-b": [_pool("shared")],
        "cluster-c": [_pool("shared")],
    }))
    return gcs


def test_strategy_first_is_sorted_by_name():
    r = ClusterResolver("test", gcs=_three_cluster_gcs())
    for _ in range(5):
        assert r.resolve("shared", strategy="first").cluster == "cluster-a"


def test_strategy_round_robin_rotates():
    r = ClusterResolver("test", gcs=_three_cluster_gcs())
    picks = [r.resolve("shared", strategy="round-robin").cluster for _ in range(6)]
    assert picks == ["cluster-a", "cluster-b", "cluster-c",
                     "cluster-a", "cluster-b", "cluster-c"]


def test_strategy_round_robin_is_per_template():
    """Two templates should each have their own cursor."""
    gcs = FakeGCS()
    gcs.set_assignments(_assn({
        "cluster-a": [_pool("t1"), _pool("t2")],
        "cluster-b": [_pool("t1"), _pool("t2")],
    }))
    r = ClusterResolver("test", gcs=gcs)
    # Interleave t1 and t2 — cursors independent.
    assert r.resolve("t1", strategy="round-robin").cluster == "cluster-a"
    assert r.resolve("t2", strategy="round-robin").cluster == "cluster-a"
    assert r.resolve("t1", strategy="round-robin").cluster == "cluster-b"
    assert r.resolve("t2", strategy="round-robin").cluster == "cluster-b"


def test_strategy_hash_is_deterministic():
    r = ClusterResolver("test", gcs=_three_cluster_gcs())
    first = r.resolve("shared", strategy="hash").cluster
    for _ in range(10):
        assert r.resolve("shared", strategy="hash").cluster == first


def test_strategy_hash_varies_by_template_name():
    r = ClusterResolver("test", gcs=FakeGCS()); r._gcs.set_assignments(_assn({
        "cluster-a": [_pool("alpha"), _pool("bravo"), _pool("charlie"),
                      _pool("delta")],
        "cluster-b": [_pool("alpha"), _pool("bravo"), _pool("charlie"),
                      _pool("delta")],
        "cluster-c": [_pool("alpha"), _pool("bravo"), _pool("charlie"),
                      _pool("delta")],
    }))
    picks = {t: r.resolve(t, strategy="hash").cluster
             for t in ["alpha", "bravo", "charlie", "delta"]}
    # At least two distinct clusters — otherwise the hash is broken.
    assert len(set(picks.values())) >= 2


def test_strategy_unknown_raises():
    r = ClusterResolver("test", gcs=_three_cluster_gcs())
    with pytest.raises(ValueError, match="unknown strategy"):
        r.resolve("shared", strategy="bogus")


# --------------------------------------------------------------------------- #
# Cache behavior
# --------------------------------------------------------------------------- #

def test_second_read_hits_cache():
    gcs = _three_cluster_gcs()
    r = ClusterResolver("test", gcs=gcs, refresh_interval_s=3600.0)
    r.resolve("shared")
    r.resolve("shared")
    r.resolve("shared")
    assert gcs.read_count == 1


def test_refresh_forces_reread():
    gcs = _three_cluster_gcs()
    r = ClusterResolver("test", gcs=gcs, refresh_interval_s=3600.0)
    r.resolve("shared")
    r.resolve("shared", refresh=True)
    assert gcs.read_count == 2


def test_cache_expires_after_interval(monkeypatch):
    import agent_sandbox_fleet.resolver as m
    ticks = [1000.0]
    monkeypatch.setattr(m.time, "time", lambda: ticks[0])
    gcs = _three_cluster_gcs()
    r = ClusterResolver("test", gcs=gcs, refresh_interval_s=60.0)
    r.resolve("shared")
    assert gcs.read_count == 1
    ticks[0] = 1030.0  # still fresh
    r.resolve("shared")
    assert gcs.read_count == 1
    ticks[0] = 1070.0  # now stale
    r.resolve("shared")
    assert gcs.read_count == 2


# --------------------------------------------------------------------------- #
# list_matches / all_templates / snapshot
# --------------------------------------------------------------------------- #

def test_list_matches_returns_all_hosting_clusters():
    gcs = _three_cluster_gcs()
    r = ClusterResolver("test", gcs=gcs, context_naming=lambda c: f"ctx-{c}")
    matches = r.list_matches("shared")
    assert [m.cluster for m in matches] == ["cluster-a", "cluster-b", "cluster-c"]
    # Every match populates context_name.
    for m in matches:
        assert m.context_name == f"ctx-{m.cluster}"


def test_all_templates_lists_every_template_once():
    gcs = FakeGCS()
    gcs.set_assignments(_assn({
        "cluster-a": [_pool("t1"), _pool("t2")],
        "cluster-b": [_pool("t1"), _pool("t3")],
    }))
    r = ClusterResolver("test", gcs=gcs)
    assert r.all_templates() == ["t1", "t2", "t3"]


def test_snapshot_returns_raw_assignments():
    gcs = FakeGCS()
    data = _assn({"cluster-a": [_pool("t1")]})
    gcs.set_assignments(data)
    r = ClusterResolver("test", gcs=gcs)
    snap = r.snapshot()
    assert snap["generation"] == 1
    assert "cluster-a" in snap["clusters"]


# --------------------------------------------------------------------------- #
# Retry / transient error handling
# --------------------------------------------------------------------------- #

def test_transient_gcs_error_retries(monkeypatch):
    gcs = _three_cluster_gcs()
    # First read raises, second succeeds.
    gcs.raise_next = RuntimeError("transient blip")
    # Zero sleep so the test is instant.
    monkeypatch.setattr("agent_sandbox_fleet.resolver.time.sleep", lambda _: None)
    r = ClusterResolver("test", gcs=gcs, retries=3, retry_base_delay_s=0.001)
    out = r.resolve("shared")
    assert out.cluster == "cluster-a"
    assert gcs.read_count == 1  # only the successful one counts


def test_all_retries_fail_raises_resolver_error(monkeypatch):
    gcs = FakeGCS()

    def always_raise(_path):
        raise RuntimeError("chronic gcs failure")
    gcs.get_json = always_raise
    monkeypatch.setattr("agent_sandbox_fleet.resolver.time.sleep", lambda _: None)
    r = ClusterResolver("test", gcs=gcs, retries=2, retry_base_delay_s=0.001)
    with pytest.raises(ResolverError, match="failed to read"):
        r.resolve("anything")


def test_assignments_missing_does_not_retry():
    """AssignmentsMissingError is terminal — no amount of retrying creates the file."""
    gcs = FakeGCS()  # get_json returns None → AssignmentsMissingError
    r = ClusterResolver("test", gcs=gcs, retries=5, retry_base_delay_s=1000.0)
    # If retry logic wrongly kicked in, this would take 1000s. If it's terminal, instant.
    with pytest.raises(AssignmentsMissingError):
        r.resolve("anything")


# --------------------------------------------------------------------------- #
# Context-naming helpers
# --------------------------------------------------------------------------- #

def test_gke_context_naming_matches_gcloud_format():
    naming = gke_context_naming("my-proj", "us-central1-a")
    assert naming("fleet-a") == "gke_my-proj_us-central1-a_fleet-a"


def test_kind_context_naming():
    naming = kind_context_naming()
    assert naming("fleet-a") == "kind-fleet-a"


def test_looks_zonal_distinguishes_zone_from_region():
    assert _looks_zonal("us-central1-a") is True
    assert _looks_zonal("us-central1") is False
    assert _looks_zonal("europe-west4-b") is True
    assert _looks_zonal("europe-west4") is False


def test_get_credentials_cmd_uses_zone_flag_for_zonal():
    from agent_sandbox_fleet.resolver import ResolvedCluster
    rc = ResolvedCluster(
        cluster="c1", template="t", warmpool="wp", replicas=1,
        image=None, generation=1, context_name="ctx",
    )
    cmd = rc.get_credentials_cmd(project="p", location="us-central1-a")
    assert "--zone us-central1-a" in cmd
    assert "--project p" in cmd


def test_get_credentials_cmd_uses_region_flag_for_regional():
    from agent_sandbox_fleet.resolver import ResolvedCluster
    rc = ResolvedCluster(
        cluster="c1", template="t", warmpool="wp", replicas=1,
        image=None, generation=1, context_name="ctx",
    )
    cmd = rc.get_credentials_cmd(project="p", location="us-central1")
    assert "--region us-central1" in cmd


# --------------------------------------------------------------------------- #
# FleetSandboxClient
# --------------------------------------------------------------------------- #

class FakeSandbox:
    """Minimal fake SandboxClient return value with a claim_name attribute."""
    def __init__(self, name: str):
        self.claim_name = name


class FakeSDKClient:
    """Records create/delete calls per instance so tests can assert routing."""
    def __init__(self, cluster_id: str):
        self.cluster_id = cluster_id
        self.created: list[tuple[str, str]] = []  # (warmpool, namespace)
        self.deleted: list[tuple[str, str]] = []  # (claim, namespace)
        self._counter = 0

    def create_sandbox(self, warmpool, namespace, **kwargs):
        self._counter += 1
        self.created.append((warmpool, namespace))
        return FakeSandbox(f"{self.cluster_id}-claim-{self._counter}")

    def delete_sandbox(self, claim_name, namespace):
        self.deleted.append((claim_name, namespace))


def _fleet_client(gcs: FakeGCS, factory_calls: list[FakeSDKClient]) -> FleetSandboxClient:
    """FleetSandboxClient with an injected factory that records what it built."""
    resolver = ClusterResolver(
        "test", gcs=gcs, context_naming=lambda c: f"ctx-{c}",
    )

    def factory():
        # The resolver has already resolved before this is called; use the length
        # of the recorded calls to tag which cluster this fake belongs to.
        cid = f"c{len(factory_calls)}"
        fake = FakeSDKClient(cid)
        factory_calls.append(fake)
        return fake

    return FleetSandboxClient(
        "test", context_naming=lambda c: f"ctx-{c}",
        resolver=resolver, sandbox_client_factory=factory,
    )


def test_fleet_client_dispatches_to_correct_cluster():
    gcs = _three_cluster_gcs()
    built: list[FakeSDKClient] = []
    fc = _fleet_client(gcs, built)
    # Round-robin: three calls hit three different clusters, so three clients built.
    fc.create_sandbox("shared", strategy="round-robin")
    fc.create_sandbox("shared", strategy="round-robin")
    fc.create_sandbox("shared", strategy="round-robin")
    assert len(built) == 3
    # Cached — 4th call reuses one of them.
    fc.create_sandbox("shared", strategy="round-robin")
    assert len(built) == 3


def test_fleet_client_records_claim_cluster_mapping():
    gcs = _three_cluster_gcs()
    built: list[FakeSDKClient] = []
    fc = _fleet_client(gcs, built)
    sb = fc.create_sandbox("shared", strategy="first")
    # Delete via just the claim name — routing must find the right client.
    fc.delete_sandbox(sb.claim_name)
    # Exactly one FakeSDKClient recorded a delete.
    deletes = [c for c in built if c.deleted]
    assert len(deletes) == 1
    assert deletes[0].deleted == [(sb.claim_name, "multi-cluster-fleet")]


def test_fleet_client_delete_unknown_claim_requires_cluster():
    gcs = _three_cluster_gcs()
    fc = _fleet_client(gcs, [])
    with pytest.raises(ResolverError, match="don't know which cluster"):
        fc.delete_sandbox("never-seen-claim")


def test_fleet_client_drops_client_on_create_failure():
    gcs = _three_cluster_gcs()

    class ExplodingSDK:
        def __init__(self):
            self.calls = 0

        def create_sandbox(self, **_):
            self.calls += 1
            raise RuntimeError("simulated auth failure")

        def delete_sandbox(self, *_, **__):
            pass

    built: list[ExplodingSDK] = []

    def factory():
        c = ExplodingSDK()
        built.append(c)
        return c

    resolver = ClusterResolver(
        "test", gcs=gcs, context_naming=lambda c: f"ctx-{c}",
    )
    fc = FleetSandboxClient(
        "test", context_naming=lambda c: f"ctx-{c}",
        resolver=resolver, sandbox_client_factory=factory,
    )
    # First call fails and drops the cached client.
    with pytest.raises(RuntimeError, match="simulated auth failure"):
        fc.create_sandbox("shared", strategy="first")
    # Second call rebuilds a fresh client for the same cluster.
    with pytest.raises(RuntimeError, match="simulated auth failure"):
        fc.create_sandbox("shared", strategy="first")
    assert len(built) == 2


def test_fleet_client_requires_context_naming_to_build():
    gcs = _three_cluster_gcs()
    # Resolver has NO context_naming — so context_name on the ResolvedCluster is None.
    resolver = ClusterResolver("test", gcs=gcs, context_naming=None)
    # Client passes context_naming to itself but the resolver-produced ResolvedCluster
    # has context_name=None → build should fail with a clear error.
    fc = FleetSandboxClient(
        "test", context_naming=lambda c: c,  # unused because resolver has None
        resolver=resolver,
        sandbox_client_factory=lambda: FakeSDKClient("x"),
    )
    with pytest.raises(ClusterUnavailableError, match="context_naming"):
        fc.create_sandbox("shared")


# --------------------------------------------------------------------------- #
# context_naming is public on the resolver.
#
# delete_sandbox needs to name a context for a cluster it did not get from
# resolve(); it used to read resolver._context_naming directly. A property
# rather than a copy on the client, so an injected resolver reports its own.
# --------------------------------------------------------------------------- #

def test_resolver_exposes_context_naming():
    naming = kind_context_naming()
    r = ClusterResolver("test", gcs=FakeGCS({}), context_naming=naming)
    assert r.context_naming is naming
    assert r.context_naming("c1") == "kind-c1"


def test_resolver_context_naming_is_none_when_unset():
    assert ClusterResolver("test", gcs=FakeGCS({})).context_naming is None


def test_delete_reuses_the_cached_client_without_racing_the_dict():
    # delete_sandbox used to read self._clients outside _client_lock, which
    # every other access on the class guards. Going through
    # _get_or_build_client keeps the lookup locked AND must not build a second
    # client for a cluster already cached.
    gcs = _three_cluster_gcs()
    built: list[FakeSDKClient] = []
    fc = _fleet_client(gcs, built)
    sb = fc.create_sandbox("shared", strategy="first")
    assert len(built) == 1

    fc.delete_sandbox(sb.claim_name)
    assert len(built) == 1, "delete built a second client instead of reusing"
    assert built[0].deleted == [(sb.claim_name, "multi-cluster-fleet")]


def test_delete_with_explicit_cluster_builds_via_resolver_naming():
    # No create first, so nothing is cached and the client must be built from
    # the resolver's context_naming.
    #
    # Deliberately NOT built with _fleet_client: that helper hands the resolver
    # and the facade the same `lambda c: f"ctx-{c}"`, and the injected factory
    # takes no arguments, so the resolved context never reaches the test. With
    # both mappers identical and the context discarded, the assertions below
    # would hold even if delete_sandbox never consulted the resolver at all --
    # which is precisely the behavior this test exists to pin.
    #
    # So: give the resolver its own recording mapper, hand the facade a
    # different one, and assert on what the resolver's was asked for.
    gcs = _three_cluster_gcs()
    asked: list[str] = []

    def resolver_naming(cluster: str) -> str:
        asked.append(cluster)
        return f"resolver-ctx-{cluster}"

    resolver = ClusterResolver("test", gcs=gcs, context_naming=resolver_naming)
    built: list[FakeSDKClient] = []

    def factory():
        fake = FakeSDKClient(f"c{len(built)}")
        built.append(fake)
        return fake

    fc = FleetSandboxClient(
        "test", context_naming=lambda c: f"facade-ctx-{c}",
        resolver=resolver, sandbox_client_factory=factory,
    )

    fc.delete_sandbox("foreign-claim", cluster="c1")

    assert asked == ["c1"], (
        "delete_sandbox did not resolve the context through the resolver's "
        "public context_naming"
    )
    assert len(built) == 1
    assert built[0].deleted == [("foreign-claim", "multi-cluster-fleet")]
