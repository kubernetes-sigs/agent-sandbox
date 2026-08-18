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
