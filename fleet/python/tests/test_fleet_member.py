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

"""Tests for agent_sandbox_fleet.fleet_member loop and cache behavior.

FleetMember.__init__ loads kube config and constructs a SandboxClient, so
these build the object with object.__new__ and populate only the attributes
the loops touch. That keeps the tests hermetic — no cluster, no bucket.
"""

from __future__ import annotations

import json
import logging
import threading

import pytest

from agent_sandbox_fleet import fleet_member
from agent_sandbox_fleet.fleet_member import Assignments, FleetMember, Paths


def _bare_member(**attrs) -> FleetMember:
    fm = object.__new__(FleetMember)
    fm.cluster_name = "test"
    fm.namespace = "multi-cluster-fleet"
    fm.reconcile_interval = 0.01
    fm.capacity_interval = 0.01
    fm.capacity_detail = "full"
    fm.paths = Paths()
    fm.hub_publisher = None
    fm._last_etag = ""
    fm._last_assignment = None
    fm._retry_pending = False
    fm._stop = threading.Event()
    for k, v in attrs.items():
        setattr(fm, k, v)
    return fm


# --------------------------------------------------------------------------- #
# The reconcile loop must survive a failing FIRST pass.
#
# The first _reconcile_once() used to sit outside the try/except. A startup
# failure — GCS IAM denial, apiserver error, malformed assignments.json —
# propagated out of the thread and killed it, while run() went on blocking on
# self._stop. The pod stays 1/1 Running and Ready and never reconciles again.
# --------------------------------------------------------------------------- #

def test_reconcile_loop_survives_a_failing_first_pass():
    calls = []
    fm = _bare_member()

    def flaky():
        calls.append(1)
        if len(calls) == 1:
            raise RuntimeError("GCS 403 at startup")
        fm._stop.set()

    fm._reconcile_once = flaky
    fm._reconcile_loop()  # must return, not raise

    assert len(calls) == 2, "loop died on the first failure instead of retrying"


def test_reconcile_loop_runs_first_pass_immediately():
    # The retry guard must not cost us the fire-once-at-startup behavior.
    calls = []
    fm = _bare_member(reconcile_interval=3600.0)

    def once():
        calls.append(1)
        fm._stop.set()

    fm._reconcile_once = once
    fm._reconcile_loop()

    assert calls == [1]


def test_reconcile_loop_exits_promptly_on_stop():
    fm = _bare_member(reconcile_interval=3600.0)
    fm._stop.set()
    fm._reconcile_once = lambda: None
    fm._reconcile_loop()  # returns without waiting out the interval


# --------------------------------------------------------------------------- #
# _fetch_assignments etag cache.
# --------------------------------------------------------------------------- #

class _FakeGCS:
    """Minimal get_with_etag; raise FileNotFoundError when `obj` is None."""

    def __init__(self):
        self.obj: tuple[bytes, str] | None = None

    def get_with_etag(self, path):
        if self.obj is None:
            raise FileNotFoundError(path)
        return self.obj


def test_fetch_assignments_clears_etag_when_object_disappears():
    gcs = _FakeGCS()
    gcs.obj = (b'{"generation": 7, "clusters": {}}', "etag-1")
    fm = _bare_member(gcs=gcs)

    assignments, changed = fm._fetch_assignments()
    assert changed is True
    assert assignments.generation == 7
    assert fm._last_etag == "etag-1"

    # Object deleted: one changed=True to signal the transition...
    gcs.obj = None
    _, changed = fm._fetch_assignments()
    assert changed is True
    assert fm._last_etag == "", "stale etag survived the delete"

    # ...and then quiet, instead of re-reporting a change every single tick.
    _, changed = fm._fetch_assignments()
    assert changed is False


def test_fetch_assignments_rereads_after_delete_and_recreate():
    # The stale-etag bug also hid a recreate: an object whose etag matched the
    # pre-delete one was read as unchanged and silently skipped.
    gcs = _FakeGCS()
    gcs.obj = (b'{"generation": 1, "clusters": {}}', "etag-1")
    fm = _bare_member(gcs=gcs)
    fm._fetch_assignments()

    gcs.obj = None
    fm._fetch_assignments()

    gcs.obj = (b'{"generation": 2, "clusters": {}}', "etag-1")
    assignments, changed = fm._fetch_assignments()
    assert changed is True
    assert assignments.generation == 2


def test_fetch_assignments_reports_unchanged_on_matching_etag():
    gcs = _FakeGCS()
    gcs.obj = (b'{"generation": 3, "clusters": {}}', "etag-1")
    fm = _bare_member(gcs=gcs)
    first, _ = fm._fetch_assignments()
    fm._last_assignment = first

    again, changed = fm._fetch_assignments()
    assert changed is False
    assert again is first


def test_fetch_assignments_missing_from_the_start_is_not_a_change():
    gcs = _FakeGCS()
    fm = _bare_member(gcs=gcs)
    assignments, changed = fm._fetch_assignments()
    assert changed is False
    assert assignments == Assignments()


# --------------------------------------------------------------------------- #
# --publish-clusterprofile preflight.
#
# The hub is a different cluster from the one the member runs in, so there is
# no in-cluster fallback. Without a path, load_kube_config() falls back to
# ~/.kube/config and either fails deep inside the client or publishes this
# cluster's capacity onto whatever hub that file points at.
# --------------------------------------------------------------------------- #

def test_publish_clusterprofile_requires_a_hub_kubeconfig(monkeypatch, capsys):
    monkeypatch.delenv("HUB_KUBECONFIG", raising=False)
    monkeypatch.setenv("CLUSTER_NAME", "a")
    monkeypatch.setenv("FLEET_BUCKET", "b")

    with pytest.raises(SystemExit):
        fleet_member.main(["--publish-clusterprofile"])
    assert "--hub-kubeconfig" in capsys.readouterr().err


def test_publish_clusterprofile_rejects_a_missing_kubeconfig(monkeypatch,
                                                             capsys, tmp_path):
    monkeypatch.setenv("CLUSTER_NAME", "a")
    monkeypatch.setenv("FLEET_BUCKET", "b")
    missing = str(tmp_path / "nope.yaml")

    with pytest.raises(SystemExit):
        fleet_member.main(["--publish-clusterprofile", "--hub-kubeconfig", missing])
    err = capsys.readouterr().err
    assert "does not exist" in err


# --------------------------------------------------------------------------- #
# A partially-applied pass must retry itself.
#
# _reconcile_once short-circuits on an unchanged etag. That is the right call
# for a pass that succeeded, and a trap for one that did not: assignments.json
# only changes when somebody runs `fleetctl apply`, so a cluster that skipped a
# pool -- template not applied yet, ApiException out of _ensure_warmpool --
# would serve the wrong pool set for as long as the plan held steady. Which,
# for a stable fleet, is forever.
# --------------------------------------------------------------------------- #

class _RecordingMember:
    """The _reconcile_once collaborators, scripted per pool name."""

    def __init__(self, missing_templates=()):
        self.missing = set(missing_templates)
        self.ensured: list[str] = []

    def install(self, fm):
        fm._template_exists = lambda t: t not in self.missing
        fm._ensure_warmpool = lambda gen, pool: self.ensured.append(pool.warmpool)
        fm._list_managed_pool_names = lambda: []
        return self


def _one_pool_gcs(template="tpl-a"):
    gcs = _FakeGCS()
    gcs.obj = (
        ('{"generation": 1, "clusters": {"test": {"pools": ['
         '{"warmpool": "wp-a", "template": "%s", "replicas": 1}]}}}' % template
         ).encode(),
        "etag-1",
    )
    return gcs


def test_a_skipped_pool_is_retried_on_the_next_tick():
    gcs = _one_pool_gcs()
    fm = _bare_member(gcs=gcs)
    helper = _RecordingMember(missing_templates={"tpl-a"}).install(fm)

    fm._reconcile_once()
    assert helper.ensured == [], "template was missing, nothing should be applied"
    assert fm._retry_pending is True

    # Same etag, same bytes -- the only thing that changed is the operator
    # applying the template. Nothing rewrites assignments.json for that.
    helper.missing.clear()
    fm._reconcile_once()
    assert helper.ensured == ["wp-a"], (
        "the unchanged etag suppressed the retry; this cluster is stuck serving "
        "the wrong pool set until somebody re-runs fleetctl apply"
    )


def test_an_exception_mid_pass_leaves_the_retry_armed():
    # Not just the tidy skip path: anything thrown between the guard and the end
    # of the method has to count as "did not finish".
    gcs = _one_pool_gcs()
    fm = _bare_member(gcs=gcs)
    helper = _RecordingMember().install(fm)

    def boom():
        raise RuntimeError("apiserver said no")

    fm._list_managed_pool_names = boom
    with pytest.raises(RuntimeError):
        fm._reconcile_once()
    assert fm._retry_pending is True

    fm._list_managed_pool_names = lambda: []
    fm._reconcile_once()
    assert helper.ensured == ["wp-a", "wp-a"], "the failed pass was never retried"


def test_a_clean_pass_does_not_re_reconcile_on_every_tick():
    # The other half of the contract. Arming the retry unconditionally would
    # turn the etag short-circuit into dead code and put a full pool sweep on
    # the apiserver every reconcile_interval, on every cluster in the fleet.
    gcs = _one_pool_gcs()
    fm = _bare_member(gcs=gcs)
    helper = _RecordingMember().install(fm)

    fm._reconcile_once()
    fm._reconcile_once()
    fm._reconcile_once()
    assert helper.ensured == ["wp-a"]
    assert fm._retry_pending is False

# --------------------------------------------------------------------------- #
# _list_managed_pool_names pages.
#
# The bug this guards is NOT truncation -- omitting `limit` makes the apiserver
# return the whole collection, so the sweep was complete. It is the single
# unbounded request: one etcd range read over every managed pool in the
# namespace plus a multi-megabyte body, every reconcile tick, on the same
# apiserver the sandbox creates queue behind. At 500 pools per cluster on the
# density fleet that is the shape of request a starved control plane times out
# on -- and a timeout here DOES skip the orphan sweep.
# --------------------------------------------------------------------------- #

class _PagingCustomObjects:
    """list_namespaced_custom_object that honours limit/_continue, like the real one."""

    def __init__(self, names, make_item=None):
        self.names = list(names)
        self._make_item = make_item or (lambda n: {"metadata": {"name": n}})
        self.calls: list[dict] = []
        self.deleted: list[str] = []

    def list_namespaced_custom_object(self, **kw):
        self.calls.append(kw)
        limit = kw.get("limit") or len(self.names) or 1
        start = int(kw.get("_continue") or 0)
        page = self.names[start:start + limit]
        nxt = start + limit
        meta = {}
        if nxt < len(self.names):
            meta["continue"] = str(nxt)
        return {"items": [self._make_item(n) for n in page], "metadata": meta}

    def delete_namespaced_custom_object(self, **kw):
        self.deleted.append(kw["name"])


def test_list_managed_pool_names_walks_every_page():
    co = _PagingCustomObjects([f"wp-{i}" for i in range(2500)])
    fm = _bare_member(custom_objects=co)

    names = fm._list_managed_pool_names()

    assert names == [f"wp-{i}" for i in range(2500)]
    assert len(co.calls) == 3, "did not page: expected 2500 names at PAGE_LIMIT=1000"
    assert all(c["limit"] == fleet_member.PAGE_LIMIT for c in co.calls)
    assert [c["_continue"] for c in co.calls] == [None, "1000", "2000"]


def test_list_managed_pool_names_bounds_a_single_request():
    # The property that matters at fleet scale: no request ever asks for more
    # than one page, however many pools the namespace holds.
    co = _PagingCustomObjects([f"wp-{i}" for i in range(5000)])
    fm = _bare_member(custom_objects=co)
    fm._list_managed_pool_names()
    assert all(c.get("limit") == fleet_member.PAGE_LIMIT for c in co.calls), (
        "an unbounded list request survived; this is the one that OOMs or "
        "times out on a loaded apiserver"
    )


def test_orphans_beyond_the_first_page_are_still_deleted():
    # The behavioral consequence, end to end through _reconcile_once: a pool the
    # plan no longer names has to be deleted no matter which page it landed on.
    co = _PagingCustomObjects([f"wp-{i}" for i in range(1500)])
    fm = _bare_member(gcs=_one_pool_gcs(), custom_objects=co)
    fm._template_exists = lambda t: True
    fm._ensure_warmpool = lambda gen, pool: None

    fm._reconcile_once()

    assert "wp-1200" in co.deleted, "an orphan on page 2 was never swept"
    assert len(co.deleted) == 1500


# --------------------------------------------------------------------------- #
# AssignmentPool forward compatibility.
#
# planner and members are separate deployments, and the planner -- a single
# writer -- is normally rolled first. AssignmentPool(**p) raised TypeError on
# any field a newer planner added, and because _reconcile_loop catches and
# retries, the member did not crash: it stayed 1/1 Ready, kept reporting
# capacity, and silently never applied another plan.
# --------------------------------------------------------------------------- #

@pytest.fixture(autouse=True)
def _reset_unknown_field_log():
    fleet_member._UNKNOWN_POOL_FIELDS.clear()
    yield
    fleet_member._UNKNOWN_POOL_FIELDS.clear()


def _plan_with(extra_json: str) -> _FakeGCS:
    gcs = _FakeGCS()
    gcs.obj = (
        ('{"generation": 9, "clusters": {"test": {"pools": ['
         '{"warmpool": "wp-a", "template": "tpl-a", "replicas": 2%s}]}}}'
         % extra_json).encode(),
        "etag-new",
    )
    return gcs


def test_a_newer_planner_field_does_not_wedge_the_member():
    fm = _bare_member(gcs=_plan_with(', "priority": "high", "tolerations": []'))

    assignments, changed = fm._fetch_assignments()

    assert changed is True
    pool = assignments.clusters["test"].pools[0]
    assert (pool.warmpool, pool.template, pool.replicas) == ("wp-a", "tpl-a", 2)


def test_known_fields_still_land():
    fm = _bare_member(gcs=_plan_with(', "image": "img:v1", "priority": 3'))
    assignments, _ = fm._fetch_assignments()
    assert assignments.clusters["test"].pools[0].image == "img:v1"


def test_an_unknown_field_is_logged_once_not_every_tick(caplog):
    caplog.set_level("WARNING", logger="agent_sandbox_fleet.fleet_member")
    for _ in range(5):
        fleet_member.AssignmentPool.from_json(
            {"warmpool": "wp-a", "template": "tpl-a", "replicas": 1,
             "priority": "high"}
        )
    warnings = [r for r in caplog.records if "priority" in r.getMessage()]
    assert len(warnings) == 1, (
        "an ignored field must be visible, but once per pod -- not once per "
        "pool per reconcile tick for the life of the deployment"
    )


def test_a_missing_required_field_still_raises():
    # The mirror case is NOT forward compatibility. A plan without `replicas` is
    # malformed or truncated, and applying half of it is worse than retrying.
    with pytest.raises(TypeError):
        fleet_member.AssignmentPool.from_json(
            {"warmpool": "wp-a", "template": "tpl-a"}
        )


# --------------------------------------------------------------------------- #
# _collect_capacity pages the SAME collection.
#
# The reconcile loop was fixed and the capacity loop was not, which is the worse
# half: capacity_interval is the more frequent of the two, and a timeout here
# does not just skip a sweep -- the report goes unwritten, and after
# max_report_age_s the planner ages this cluster out of placement entirely.
# Both readers now go through _iter_managed_pools so there is one walk to get
# right.
# --------------------------------------------------------------------------- #

def _pool_item(name):
    gen = int(name.split("-")[1])
    return {
        "metadata": {
            "name": name,
            "annotations": {fleet_member.GENERATION_ANNOTATION: str(gen)},
        },
        "status": {"replicas": 2, "readyReplicas": 1},
    }


def test_collect_capacity_aggregates_across_pages():
    co = _PagingCustomObjects([f"wp-{i}" for i in range(1500)],
                              make_item=_pool_item)
    fm = _bare_member(custom_objects=co, capacity_detail="light")

    report = fm._collect_capacity()

    assert len(co.calls) == 2, "capacity read the pool list in one unbounded call"
    assert report.warmpool_depth == 3000
    assert report.warmpool_ready == 1500
    assert report.generation_observed == 1499
    assert len(report.reported_pools) == 1500


def test_collect_capacity_never_issues_an_unbounded_list():
    co = _PagingCustomObjects([f"wp-{i}" for i in range(5000)],
                              make_item=_pool_item)
    fm = _bare_member(custom_objects=co, capacity_detail="light")
    fm._collect_capacity()
    assert all(c.get("limit") == fleet_member.PAGE_LIMIT for c in co.calls), (
        "an unbounded pool list survived on the capacity path, which runs more "
        "often than reconcile"
    )


def test_both_readers_share_one_paged_walk():
    # The property that keeps this fixed: neither caller builds its own list
    # request, so a future third reader cannot regress independently.
    co = _PagingCustomObjects([f"wp-{i}" for i in range(1200)],
                              make_item=_pool_item)
    fm = _bare_member(custom_objects=co, capacity_detail="light")

    names = fm._list_managed_pool_names()
    report = fm._collect_capacity()

    # reported_pools is sorted for a stable wire payload; the sweep keeps
    # apiserver order. Same set, which is the invariant that matters.
    assert sorted(names) == report.reported_pools
    assert all(c.get("limit") == fleet_member.PAGE_LIMIT for c in co.calls)


def test_iter_managed_pools_holds_one_page_not_the_whole_collection():
    # A generator, not list[dict]: paging the wire and then materialising every
    # pool body in memory would defeat the point of PAGE_LIMIT.
    co = _PagingCustomObjects([f"wp-{i}" for i in range(2500)],
                              make_item=_pool_item)
    fm = _bare_member(custom_objects=co)

    it = fm._iter_managed_pools()
    next(it)
    assert len(co.calls) == 1, "the walk ran to completion before yielding"


# --------------------------------------------------------------------------- #
# schema_version gate.
#
# The failure this prevents: an unparseable payload yields no clusters, an empty
# pool set for this cluster, and an empty pool set means "drop everything". So
# without the gate, a schema bump tears down the fleet it is rolling out to.
# --------------------------------------------------------------------------- #

def _assignment_bytes(version, generation, pools=()):
    body = {
        "generation": generation,
        "clusters": {"c1": {"pools": [
            {"template": t, "warmpool": f"{t}-pool", "replicas": 1} for t in pools
        ]}},
    }
    if version is not None:
        body["schema_version"] = version
    return json.dumps(body).encode()


def test_a_payload_with_a_known_schema_version_is_applied():
    gcs = _FakeGCS()
    gcs.obj = (_assignment_bytes(fleet_member.SCHEMA_VERSION, 4, ["t1"]), "etag-1")
    fm = _bare_member(gcs=gcs)
    assignments, changed = fm._fetch_assignments()
    assert changed is True
    assert assignments.schema_version == fleet_member.SCHEMA_VERSION
    assert [p.template for p in assignments.clusters["c1"].pools] == ["t1"]


def test_a_payload_with_no_schema_version_is_treated_as_the_current_one():
    # Backward compatibility with plans written before the field existed. They
    # are shape-identical to v1, so refusing them would strand a running fleet
    # on the first upgrade.
    gcs = _FakeGCS()
    gcs.obj = (_assignment_bytes(None, 4, ["t1"]), "etag-1")
    fm = _bare_member(gcs=gcs)
    assignments, changed = fm._fetch_assignments()
    assert changed is True
    assert [p.template for p in assignments.clusters["c1"].pools] == ["t1"]


def test_an_unknown_schema_version_is_refused_without_dropping_pools(caplog):
    gcs = _FakeGCS()
    gcs.obj = (_assignment_bytes(fleet_member.SCHEMA_VERSION, 4, ["t1"]), "etag-1")
    fm = _bare_member(gcs=gcs)
    good, _ = fm._fetch_assignments()
    fm._last_assignment = good

    gcs.obj = (_assignment_bytes(999, 5, []), "etag-2")
    with caplog.at_level(logging.ERROR):
        assignments, changed = fm._fetch_assignments()

    assert changed is False, "an unparseable plan must not trigger a reconcile"
    assert assignments is good, "the member must keep serving its current pools"
    assert [p.template for p in assignments.clusters["c1"].pools] == ["t1"]
    assert "REFUSING" in caplog.text


def test_a_refused_payload_does_not_cache_its_etag(caplog):
    # Caching it would make the next tick read "unchanged" and go quiet, so the
    # fleet would sit stuck with one log line and no ongoing signal -- and a
    # corrected plan at the same etag would never be re-read.
    gcs = _FakeGCS()
    gcs.obj = (_assignment_bytes(999, 5, []), "etag-bad")
    fm = _bare_member(gcs=gcs)
    with caplog.at_level(logging.ERROR):
        fm._fetch_assignments()
        assert fm._last_etag == ""
        caplog.clear()
        fm._fetch_assignments()
        assert "REFUSING" in caplog.text, "the refusal must stay loud every tick"


def test_a_refusal_on_a_fresh_start_does_not_reconcile_at_all(caplog):
    # The pod restarted mid schema rollout: _last_assignment is None and the
    # first payload this process ever sees is one it cannot parse. The refused
    # path used to substitute an empty Assignments(), and the first-pass clause
    # in _reconcile_once reconciled it anyway -- sweeping every managed pool as
    # orphaned and caching the empty set as last-good. A fleet teardown,
    # triggered by a restart.
    gcs = _FakeGCS()
    gcs.obj = (_assignment_bytes(999, 5, []), "etag-bad")
    fm = _bare_member(gcs=gcs)

    with caplog.at_level(logging.ERROR):
        assignments, changed = fm._fetch_assignments()
    assert assignments is None, "there is no last-good assignment to fall back on"
    assert changed is False
    assert "REFUSING" in caplog.text

    deleted: list[str] = []

    class _Recorder:
        def delete_namespaced_custom_object(self, **kw):
            deleted.append(kw["name"])

    fm.custom_objects = _Recorder()
    fm._template_exists = lambda t: True
    fm._ensure_warmpool = lambda gen, pool: None
    fm._list_managed_pool_names = lambda: ["pool-from-previous-pod"]

    fm._reconcile_once()
    assert deleted == [], "the previous pod's pools were swept as orphans"
    assert fm._last_assignment is None, (
        "an empty set must not be cached as last-good"
    )


def test_a_corrected_plan_is_picked_up_after_a_refusal():
    gcs = _FakeGCS()
    gcs.obj = (_assignment_bytes(999, 5, []), "etag-bad")
    fm = _bare_member(gcs=gcs)
    fm._fetch_assignments()

    gcs.obj = (_assignment_bytes(fleet_member.SCHEMA_VERSION, 6, ["t2"]), "etag-ok")
    assignments, changed = fm._fetch_assignments()
    assert changed is True
    assert assignments.generation == 6
    assert [p.template for p in assignments.clusters["c1"].pools] == ["t2"]
