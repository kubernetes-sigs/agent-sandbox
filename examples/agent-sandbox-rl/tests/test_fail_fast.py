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

"""Fail-fast: pod classification, the claim watchdog, acquire() wiring,
handle liveness, and the reaper's orphan sweep."""

import threading
from unittest.mock import MagicMock

import pytest
from kubernetes import client

from agent_sandbox_rl import (
    ClusterRegistry,
    FailFastPolicy,
    FleetConfig,
    SandboxFleet,
    SandboxLostError,
    SandboxStartError,
    Task,
)
from agent_sandbox_rl import constants
from agent_sandbox_rl import handles as handles_mod
from agent_sandbox_rl import reaper as reaper_mod
from agent_sandbox_rl.failfast import ClaimWatchdog, classify_pod
from agent_sandbox_rl.handles import SandboxHandle

POLICY = FailFastPolicy(grace_s=60, unschedulable_grace_s=180)
FAST = FailFastPolicy(initial_delay_s=0, poll_s=0.01, grace_s=0, unschedulable_grace_s=0)


def _pod(*, phase="Pending", waiting=None, terminated=None, last_terminated=None,
         init=False, unschedulable=False, deleting=False, name="pod-1"):
  cs = {"name": "agent-runtime", "state": {}}
  if waiting:
    cs["state"]["waiting"] = {"reason": waiting, "message": f"{waiting} msg"}
  if terminated:
    cs["state"]["terminated"] = {"reason": terminated, "exit_code": 137}
  if last_terminated:
    cs["last_state"] = {"terminated": {"reason": last_terminated, "exit_code": 137}}
  status = {"phase": phase,
            ("init_container_statuses" if init else "container_statuses"): [cs]}
  if unschedulable:
    status["conditions"] = [{"type": "PodScheduled", "status": "False",
                             "reason": "Unschedulable", "message": "0/3 nodes"}]
  meta = {"name": name}
  if deleting:
    meta["deletion_timestamp"] = "2026-09-29T00:00:00Z"
  return {"metadata": meta, "status": status}


# --- classify_pod ---------------------------------------------------------- #

def test_classify_healthy_pending_pod_is_none():
  assert classify_pod(_pod(), now=0, first_seen={}, policy=POLICY) is None
  assert classify_pod(_pod(phase="Running"), now=0, first_seen={}, policy=POLICY) is None


def test_classify_terminal_waiting_reason_fails_immediately():
  v = classify_pod(_pod(waiting="InvalidImageName"), now=0, first_seen={}, policy=POLICY)
  assert v is not None and v.reason == "InvalidImageName" and v.pod_name == "pod-1"


def test_classify_retryable_reason_waits_for_grace():
  seen = {}
  pod = _pod(waiting="ImagePullBackOff")
  assert classify_pod(pod, now=0, first_seen=seen, policy=POLICY) is None
  assert classify_pod(pod, now=59, first_seen=seen, policy=POLICY) is None
  v = classify_pod(pod, now=60, first_seen=seen, policy=POLICY)
  assert v is not None and v.reason == "ImagePullBackOff" and "60s" in v.message


def test_classify_grace_restarts_when_reason_clears():
  seen = {}
  pod = _pod(waiting="ImagePullBackOff")
  classify_pod(pod, now=0, first_seen=seen, policy=POLICY)
  # the reason clears (image pulled, container creating) then comes back
  classify_pod(_pod(waiting="ContainerCreating"), now=30, first_seen=seen, policy=POLICY)
  assert seen == {}
  assert classify_pod(pod, now=80, first_seen=seen, policy=POLICY) is None   # 0s held again
  assert classify_pod(pod, now=140, first_seen=seen, policy=POLICY) is not None


def test_classify_oom_killed_init_container_is_terminal():
  v = classify_pod(_pod(terminated="OOMKilled", init=True), now=0, first_seen={}, policy=POLICY)
  assert v is not None and v.reason == "OOMKilled"
  v = classify_pod(_pod(last_terminated="OOMKilled"), now=0, first_seen={}, policy=POLICY)
  assert v is not None and v.reason == "OOMKilled"


def test_classify_failed_phase_and_deleting_pod():
  assert classify_pod(_pod(phase="Failed"), now=0, first_seen={}, policy=POLICY).reason == "PodFailed"
  assert classify_pod(_pod(deleting=True), now=0, first_seen={}, policy=POLICY).reason == "PodTerminating"


def test_classify_unschedulable_uses_its_own_grace():
  seen = {}
  pod = _pod(unschedulable=True)
  assert classify_pod(pod, now=0, first_seen=seen, policy=POLICY) is None
  assert classify_pod(pod, now=179, first_seen=seen, policy=POLICY) is None
  v = classify_pod(pod, now=180, first_seen=seen, policy=POLICY)
  assert v is not None and v.reason == "Unschedulable" and "0/3 nodes" in v.message


def test_classify_accepts_kubernetes_model():
  pod = client.V1Pod(metadata=client.V1ObjectMeta(name="m"),
                     status=client.V1PodStatus(phase="Failed", reason="Evicted"))
  v = classify_pod(pod, now=0, first_seen={}, policy=POLICY)
  assert v is not None and v.reason == "PodFailed" and v.pod_name == "m"


# --- ClaimWatchdog --------------------------------------------------------- #

def _cluster_with_claim(sandbox_name="sb-1", pod_annotation=None, sandbox_404=False):
  c = MagicMock()
  c.namespace = "ns"

  def _get(group, version, namespace, plural, name, **_kw):
    if plural == "sandboxclaims":
      return {"status": {"sandbox": {"name": sandbox_name}}}
    if sandbox_404:
      raise client.ApiException(status=404)
    ann = {"agents.x-k8s.io/pod-name": pod_annotation} if pod_annotation else {}
    return {"metadata": {"annotations": ann}}
  c.custom_api.get_namespaced_custom_object.side_effect = _get
  return c


def test_watchdog_resolves_pod_name_from_claim_and_sandbox_annotation():
  w = ClaimWatchdog(_cluster_with_claim(pod_annotation="sb-1-pod"), "claim-1", FAST)
  assert w._resolve_pod_name() == "sb-1-pod"
  w = ClaimWatchdog(_cluster_with_claim(sandbox_404=True), "claim-1", FAST)
  assert w._resolve_pod_name() == "sb-1"                 # sandbox CR not there yet


def test_watchdog_returns_none_while_claim_is_unbound_or_gone():
  c = _cluster_with_claim()
  c.custom_api.get_namespaced_custom_object.side_effect = lambda **kw: {"status": {}}
  assert ClaimWatchdog(c, "claim-1", FAST)._resolve_pod_name() is None
  c.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  assert ClaimWatchdog(c, "claim-1", FAST)._resolve_pod_name() is None


def test_watchdog_deletes_claim_on_terminal_pod():
  c = _cluster_with_claim()
  c.core_api.read_namespaced_pod_status.return_value = _pod(waiting="ImagePullBackOff")
  w = ClaimWatchdog(c, "claim-1", FAST)
  w.start()
  w.join(timeout=5)
  assert not w.is_alive()
  assert w.verdict is not None and w.verdict.reason == "ImagePullBackOff"
  c.resources.delete_claim.assert_called_once_with("claim-1", request_timeout=(3, 10))


def test_watchdog_survives_api_errors_and_stops_on_request():
  c = _cluster_with_claim()
  c.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=500)
  w = ClaimWatchdog(c, "claim-1", FAST)
  w.start()
  threading.Event().wait(0.05)
  assert w.is_alive() and w.verdict is None
  assert w.stop() is None
  w.join(timeout=5)
  assert not w.is_alive()
  c.resources.delete_claim.assert_not_called()


# --- acquire() wiring ------------------------------------------------------ #

class _Recording(ClaimWatchdog):
  instances: list = []

  def __init__(self, *a, **kw):
    super().__init__(*a, **kw)
    _Recording.instances.append(self)


@pytest.fixture
def recording_watchdog(monkeypatch):
  _Recording.instances = []
  monkeypatch.setattr("agent_sandbox_rl.fleet.ClaimWatchdog", _Recording)
  return _Recording


def _fleet(registry, **cfg):
  cfg.setdefault("install_teardown_hooks", False)
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def test_acquire_names_the_claim_and_stops_the_watchdog_on_success(make_cluster, recording_watchdog):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), claim_timeout=123)
  f.load_tasks(["img"])
  h = f.acquire(f.tasks[0])
  kw = c.sandbox_client.create_sandbox.call_args.kwargs
  assert kw["claim_name"].startswith("sandbox-claim-") and kw["sandbox_ready_timeout"] == 123
  assert len(recording_watchdog.instances) == 1
  w = recording_watchdog.instances[0]
  assert w.claim_name == kw["claim_name"] and w._stop_evt.is_set()
  assert f.handles() == [h]


def test_claim_timeout_defaults_to_ready_timeout(make_cluster, recording_watchdog):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), ready_timeout=321)
  f.load_tasks(["img"])
  f.acquire(f.tasks[0])
  assert c.sandbox_client.create_sandbox.call_args.kwargs["sandbox_ready_timeout"] == 321


def test_fail_fast_disabled_starts_no_watchdog(make_cluster, recording_watchdog):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), fail_fast=FailFastPolicy(enabled=False))
  f.load_tasks(["img"])
  f.acquire(f.tasks[0])
  assert recording_watchdog.instances == []
  assert "claim_name" in c.sandbox_client.create_sandbox.call_args.kwargs


def test_acquire_raises_sandbox_start_error_when_watchdog_kills_the_claim(make_cluster):
  c = make_cluster("solo")
  c.custom_api = _cluster_with_claim().custom_api
  c.core_api.read_namespaced_pod_status.return_value = _pod(waiting="ImagePullBackOff",
                                                            name="sb-1")
  deleted = threading.Event()
  c.resources.delete_claim.side_effect = lambda name, **kw: deleted.set()

  def _blocking_create(**kw):
    # Stand-in for the SDK's claim watch: returns only when the claim is gone.
    assert deleted.wait(5), "watchdog never deleted the claim"
    raise RuntimeError(f"SandboxClaim '{kw['claim_name']}' was deleted while waiting")
  c.sandbox_client.create_sandbox.side_effect = _blocking_create

  f = _fleet(ClusterRegistry([c]), fail_fast=FAST)
  f.load_tasks(["img"])
  with pytest.raises(SandboxStartError) as ei:
    f.acquire(f.tasks[0])
  err = ei.value
  claim = c.sandbox_client.create_sandbox.call_args.kwargs["claim_name"]
  assert err.reason == "ImagePullBackOff" and err.claim_name == claim
  assert err.pod_name == "sb-1" and err.image == "img"
  assert isinstance(err.__cause__, RuntimeError)
  assert [k.args[0] for k in c.resources.delete_claim.call_args_list] == [claim]   # once, by the watchdog
  # bookkeeping rolled back like any other failed acquire
  assert f.handles() == [] and c.active_claims == 0 and c.active_replicas == 0
  assert f._claims_reserved == 0


def test_acquire_deletes_named_claim_when_create_fails_without_verdict(make_cluster, recording_watchdog):
  c = make_cluster("solo")
  c.sandbox_client.create_sandbox.side_effect = RuntimeError("boom")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  with pytest.raises(RuntimeError):
    f.acquire(f.tasks[0])
  claim = c.sandbox_client.create_sandbox.call_args.kwargs["claim_name"]
  c.resources.delete_claim.assert_called_once_with(claim)
  assert recording_watchdog.instances[0]._stop_evt.is_set()


def test_acquire_leaves_foreign_claim_alone_on_409(make_cluster):
  c = make_cluster("solo")
  c.sandbox_client.create_sandbox.side_effect = client.ApiException(status=409)
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  with pytest.raises(client.ApiException):
    f.acquire(f.tasks[0])
  c.resources.delete_claim.assert_not_called()


# --- SandboxHandle.is_alive / SandboxLostError ------------------------------ #

def _handle(core=None):
  cluster = MagicMock()
  cluster.namespace = "ns"
  cluster.exec_core_api.return_value = core if core is not None else MagicMock()
  return SandboxHandle(task=Task(id="t", image="i"), cluster_name="c", claim_name="cl",
                       sandbox_id="s", pod_name="p", hostname="s", _cluster=cluster)


def test_is_alive_reads_the_pod_once():
  core = MagicMock()
  core.read_namespaced_pod_status.side_effect = client.ApiException(status=404)
  assert _handle(core).is_alive() is False
  core.read_namespaced_pod_status.side_effect = None
  core.read_namespaced_pod_status.return_value = client.V1Pod(
      metadata=client.V1ObjectMeta(name="p"), status=client.V1PodStatus(phase="Running"))
  assert _handle(core).is_alive() is True
  core.read_namespaced_pod_status.return_value = _pod(phase="Running", terminated="Error")
  assert _handle(core).is_alive() is False
  core.read_namespaced_pod_status.return_value = _pod(phase="Failed")
  assert _handle(core).is_alive() is False


def test_is_alive_without_cluster_is_true():
  h = SandboxHandle(task=Task(id="t", image="i"), cluster_name="c", claim_name="cl",
                    sandbox_id="s", pod_name="p", hostname="s")
  assert h.is_alive() is True


def test_exec_raises_sandbox_lost_when_pod_is_gone(monkeypatch):
  core = MagicMock()
  core.read_namespaced_pod_status.side_effect = client.ApiException(status=404)
  h = _handle(core)
  monkeypatch.setattr(handles_mod, "exec_in_pod",
                      MagicMock(side_effect=client.ApiException(status=404)))
  with pytest.raises(SandboxLostError) as ei:
    h.exec("true")
  assert ei.value.pod_name == "p" and isinstance(ei.value.cause, client.ApiException)


def test_exec_reraises_original_error_when_pod_is_alive(monkeypatch):
  core = MagicMock()
  core.read_namespaced_pod_status.return_value = _pod(phase="Running")
  h = _handle(core)
  monkeypatch.setattr(handles_mod, "exec_in_pod",
                      MagicMock(side_effect=client.ApiException(status=500)))
  with pytest.raises(client.ApiException):
    h.exec("true")


def test_exec_over_session_converts_closed_socket_on_dead_pod():
  core = MagicMock()
  core.read_namespaced_pod_status.side_effect = client.ApiException(status=404)
  h = _handle(core)
  sess = MagicMock()
  sess.is_open = True
  sess.run.side_effect = TimeoutError("session.run timed out/closed on p")
  h._session = sess
  with pytest.raises(SandboxLostError):
    h.exec("true")
  sess.close.assert_called_once()          # stale session dropped with the pod
  assert h._session is None


# --- reaper: orphan sweep --------------------------------------------------- #

def _obj(run_id, created):
  return {"metadata": {"name": f"o-{run_id}", "creationTimestamp": created,
                       "labels": {"agents.x-k8s.io/asrl-run-id": run_id}}}


def _fake_cluster(objs_by_plural):
  c = MagicMock()
  c.namespace = "ns"
  c.resources._list_objects.side_effect = lambda plural, sel=None: objs_by_plural.get(plural, [])
  return c


def test_find_orphan_runs_skips_alive_and_young_runs():
  from datetime import datetime, timedelta, timezone
  now = datetime(2026, 9, 29, 12, 0, tzinfo=timezone.utc)
  old = (now - timedelta(minutes=30)).isoformat()
  young = (now - timedelta(minutes=1)).isoformat()
  c = _fake_cluster({
      "sandboxwarmpools": [_obj("alive", old), _obj("dead", old), _obj("fresh", young)],
      "sandboxtemplates": [_obj("dead", young)],   # newest object of "dead" is young
      "sandboxclaims": [_obj("gone", old), {"metadata": {"name": "unlabelled"}}],
  })
  found = reaper_mod.find_orphan_runs(c, {"alive"}, min_age_s=300, now=now)
  assert set(found) == {"gone"}
  found = reaper_mod.find_orphan_runs(c, {"alive"}, min_age_s=0, now=now)
  assert set(found) == {"dead", "fresh", "gone"} and found["dead"]["objects"] == 2


def test_reap_orphans_reaps_each_orphan_and_honours_dry_run(monkeypatch):
  c = _fake_cluster({})
  monkeypatch.setattr(reaper_mod, "_cluster", lambda **kw: c)
  monkeypatch.setattr(reaper_mod, "find_orphan_runs",
                      lambda cluster, alive, min_age_s: {"r1": {"objects": 2}, "r2": {"objects": 1}})
  reaped = []
  protect = {}
  monkeypatch.setattr(reaper_mod, "_reap_with_cluster",
                      lambda cluster, rid, delete_pods, protect_claims_of=None: (
                          protect.__setitem__(rid, protect_claims_of),
                          reaped.append((rid, delete_pods)))[1] or {"claims": 1})
  out = reaper_mod.reap_orphans(["a"], namespace="ns", dry_run=True)
  assert out == {"r1": {"objects": 2}, "r2": {"objects": 1}} and reaped == []
  out = reaper_mod.reap_orphans(["a"], namespace="ns", delete_pods=False)
  assert reaped == [("r1", False), ("r2", False)] and out == {"r1": {"claims": 1}, "r2": {"claims": 1}}
  # claims of runs being reaped do not protect; anyone else's (or unlabelled) do
  assert protect["r1"]("r2") is False and protect["r1"]("a") is True
  assert protect["r1"](None) is True


def test_reaper_cli_orphans_flags(monkeypatch, capsys):
  calls = {}
  monkeypatch.setattr(reaper_mod, "reap_orphans",
                      lambda alive, **kw: calls.update(alive=alive, **kw) or {})
  reaper_mod.main(["--orphans", "--alive-run-ids", "a, b", "--namespace", "ns",
                   "--min-age-s", "60", "--dry-run"])
  assert calls["alive"] == ["a", "b"] and calls["namespace"] == "ns"
  assert calls["min_age_s"] == 60 and calls["dry_run"] is True
  with pytest.raises(SystemExit):
    reaper_mod.main(["--orphans", "--namespace", "ns"])   # alive list is mandatory


# --- reaper: pods and Sandboxes are reached through pools and claims ---------- #

def _linked_cluster():
  c = MagicMock()
  c.namespace = "ns"
  objs = {
      "sandboxwarmpools": [{"metadata": {"name": "pool-a"},
                            "status": {"selector": "agents.x-k8s.io/warm-pool-sandbox=aaaa"}},
                           {"metadata": {"name": "pool-b"}, "status": {}}],
      "sandboxclaims": [{"metadata": {"name": "claim-1"},
                         "status": {"sandbox": {"name": "sb-1"}}},
                        {"metadata": {"name": "claim-2"}, "status": {}}],
      "sandboxes": [{"metadata": {"name": "sb-1"},
                     "status": {"labelSelector": "agents.x-k8s.io/sandbox-name-hash=1111"}},
                    {"metadata": {"name": "sb-pool-a",
                                  "labels": {"agents.x-k8s.io/warm-pool-sandbox": "aaaa"}},
                     "status": {"labelSelector": "agents.x-k8s.io/sandbox-name-hash=2222"}},
                    {"metadata": {"name": "other"},
                     "status": {"labelSelector": "agents.x-k8s.io/sandbox-name-hash=9999"}}],
  }

  def _list(plural, sel=None, **kw):
    items = objs.get(plural, [])
    if plural == "sandboxes" and sel:          # label-selected listing, like the API
      key, _, val = sel.partition("=")
      return [o for o in items if (o["metadata"].get("labels") or {}).get(key) == val]
    return items
  c.resources._list_objects.side_effect = _list
  c.resources.list_claims.return_value = ["claim-1", "claim-2"]
  c.resources.list_warmpools.return_value = ["pool-a", "pool-b"]
  c.resources.list_sandboxes.return_value = []          # no run-id label on Sandboxes
  c.resources.list_templates.return_value = ["tpl-1"]
  return c


def test_sandbox_links_collects_pool_and_sandbox_selectors():
  sels, names = reaper_mod._sandbox_links(_linked_cluster(), "agents.x-k8s.io/asrl-run-id=r")
  assert sels == {"agents.x-k8s.io/warm-pool-sandbox=aaaa",
                  "agents.x-k8s.io/sandbox-name-hash=1111"}
  assert names == {"sb-1", "sb-pool-a"}      # claimed, plus the pool's unclaimed member


def test_reap_deletes_claimed_sandboxes_and_sweeps_pods_by_every_selector():
  c = _linked_cluster()
  counts = reaper_mod._reap_with_cluster(c, "r", delete_pods=True)
  assert sorted(k.args[0] for k in c.resources.delete_sandbox.call_args_list) == ["sb-1", "sb-pool-a"]
  swept = sorted(k.kwargs["label_selector"]
                 for k in c.core_api.delete_collection_namespaced_pod.call_args_list)
  assert swept == [f"{constants.POD_RUN_ID_LABEL}=r",
                   "agents.x-k8s.io/sandbox-name-hash=1111",
                   "agents.x-k8s.io/warm-pool-sandbox=aaaa"]
  assert counts["sandboxes"] == 2 and counts["claims"] == 2 and counts["warmpools"] == 2
  assert counts["pods"].startswith("requested (force)")


def test_reap_links_are_best_effort():
  c = _linked_cluster()
  c.resources._list_objects.side_effect = client.ApiException(status=500)
  counts = reaper_mod._reap_with_cluster(c, "r", delete_pods=True)
  c.resources.delete_sandbox.assert_not_called()
  # the label-based sweep still ran
  sels = [k.kwargs["label_selector"] for k in c.core_api.delete_collection_namespaced_pod.call_args_list]
  assert sels == [f"{constants.POD_RUN_ID_LABEL}=r"] and counts["claims"] == 2


# --- review round 1: stop-versus-delete race, cleanup retry, CLI guard -------- #

def test_watchdog_stop_waits_for_inflight_decision_and_reports_it():
  c = _cluster_with_claim()
  entered, release = threading.Event(), threading.Event()

  def _slow_delete(name, **kw):
    entered.set()
    assert release.wait(5)
  c.resources.delete_claim.side_effect = _slow_delete
  w = ClaimWatchdog(c, "claim-1", FAST)
  w._check = lambda: classify_pod(_pod(waiting="InvalidImageName"), now=0, first_seen={}, policy=FAST)
  w.start()
  assert entered.wait(5)
  result = {}
  stopper = threading.Thread(target=lambda: result.setdefault("v", w.stop()))
  stopper.start()
  stopper.join(0.2)
  assert stopper.is_alive()                  # stop() waits for the delete to finish
  release.set()
  stopper.join(5)
  assert result["v"] is not None and result["v"].reason == "InvalidImageName"
  assert w.deleted_claim is True


def test_watchdog_never_deletes_after_stop():
  c = _cluster_with_claim()
  w = ClaimWatchdog(c, "claim-1", FAST)

  def _check_then_stop():
    v = classify_pod(_pod(waiting="InvalidImageName"), now=0, first_seen={}, policy=FAST)
    w.stop()                                 # acquire() finished while we were polling
    return v
  w._check = _check_then_stop
  w.start()
  w.join(5)
  c.resources.delete_claim.assert_not_called()
  assert w.verdict is None and w.deleted_claim is False


class _IdleWatchdog(_Recording):
  """Records itself but never polls; tests set its verdict by hand."""

  def run(self):
    return None


@pytest.fixture
def idle_watchdog(monkeypatch):
  _IdleWatchdog.instances = []
  _Recording.instances = _IdleWatchdog.instances
  monkeypatch.setattr("agent_sandbox_rl.fleet.ClaimWatchdog", _IdleWatchdog)
  return _IdleWatchdog


def test_acquire_reports_start_error_when_watchdog_wins_at_the_finish(make_cluster, idle_watchdog):
  from agent_sandbox_rl.failfast import StartVerdict
  c = make_cluster("solo")
  sb = MagicMock()
  sb.claim_name = "cl"; sb.sandbox_id = "s"
  sb.get_pod_name.return_value = "p"

  def _create(**kw):
    w = idle_watchdog.instances[-1]
    w.verdict = StartVerdict("OOMKilled", "init container", "p")
    w.deleted_claim = True
    return sb
  c.sandbox_client.create_sandbox.side_effect = _create
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  with pytest.raises(SandboxStartError) as ei:
    f.acquire(f.tasks[0])
  assert ei.value.reason == "OOMKilled"
  sb.terminate.assert_called_once()          # the handle is not returned; its sandbox is torn down
  c.resources.delete_claim.assert_not_called()   # the watchdog's delete was confirmed
  assert f.handles() == [] and c.active_claims == 0 and f._claims_reserved == 0


def test_acquire_retries_claim_cleanup_when_watchdog_delete_failed(make_cluster, idle_watchdog):
  from agent_sandbox_rl.failfast import StartVerdict
  c = make_cluster("solo")

  def _create(**kw):
    w = idle_watchdog.instances[-1]
    w.verdict = StartVerdict("ImagePullBackOff", "pull", "p")
    w.deleted_claim = False                  # its delete_claim raised
    raise RuntimeError("claim wait timed out")
  c.sandbox_client.create_sandbox.side_effect = _create
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  with pytest.raises(SandboxStartError):
    f.acquire(f.tasks[0])
  claim = c.sandbox_client.create_sandbox.call_args.kwargs["claim_name"]
  c.resources.delete_claim.assert_called_once_with(claim)


def test_reaper_cli_rejects_dry_run_outside_orphan_mode(monkeypatch):
  called = []
  monkeypatch.setattr(reaper_mod, "reap", lambda *a, **kw: called.append(1) or {})
  for argv in (["--run-id", "r", "--dry-run"], ["--all", "--dry-run"]):
    with pytest.raises(SystemExit):
      reaper_mod.main(argv)
  assert called == []


# --- review round 2 ----------------------------------------------------------- #

def test_pull_reason_flips_share_one_grace():
  seen, last = {}, {}
  flips = ["ImagePullBackOff", "ErrImagePull", "ImagePullBackOff", "ErrImagePull",
           "ImagePullBackOff", "ErrImagePull", "ImagePullBackOff"]
  verdicts = [classify_pod(_pod(waiting=r), now=10 * i, first_seen=seen,
                           last_seen=last, policy=POLICY) for i, r in enumerate(flips)]
  assert verdicts[:6] == [None] * 6
  assert verdicts[6] is not None and verdicts[6].reason == "ImagePullBackOff"   # at 60 s


def test_crash_loop_keeps_its_grace_across_a_running_blip():
  seen, last = {}, {}
  assert classify_pod(_pod(waiting="CrashLoopBackOff"), now=0, first_seen=seen,
                      last_seen=last, policy=POLICY) is None
  running = _pod(phase="Running")
  running["status"]["container_statuses"][0]["state"] = {"running": {"startedAt": "x"}}
  assert classify_pod(running, now=20, first_seen=seen, last_seen=last, policy=POLICY) is None
  v = classify_pod(_pod(waiting="CrashLoopBackOff"), now=60, first_seen=seen,
                   last_seen=last, policy=POLICY)
  assert v is not None and v.reason == "CrashLoopBackOff"


def test_a_condition_absent_for_a_full_grace_period_clears():
  seen, last = {}, {}
  classify_pod(_pod(waiting="ImagePullBackOff"), now=0, first_seen=seen, last_seen=last,
               policy=POLICY)
  classify_pod(_pod(waiting="ContainerCreating"), now=61, first_seen=seen, last_seen=last,
               policy=POLICY)
  assert seen == {} and last == {}
  assert classify_pod(_pod(waiting="ImagePullBackOff"), now=100, first_seen=seen,
                      last_seen=last, policy=POLICY) is None      # grace starts over


def test_earlier_oom_kill_does_not_fail_a_running_container():
  pod = _pod(phase="Running", last_terminated="OOMKilled")
  pod["status"]["container_statuses"][0]["state"] = {"running": {"startedAt": "x"}}
  assert classify_pod(pod, now=0, first_seen={}, policy=POLICY) is None
  # a V1Pod's to_dict() carries every state key, with None for the unset ones
  model_like = _pod(waiting="CrashLoopBackOff", last_terminated="OOMKilled")
  model_like["status"]["container_statuses"][0]["state"].update(running=None, terminated=None)
  v = classify_pod(model_like, now=0, first_seen={}, policy=POLICY)
  assert v is not None and v.reason == "OOMKilled"


def test_reap_rejects_an_empty_run_id(monkeypatch):
  monkeypatch.setattr(reaper_mod, "_cluster", lambda **kw: pytest.fail("must not connect"))
  with pytest.raises(ValueError, match="non-empty run_id"):
    reaper_mod.reap("", namespace="ns")
  with pytest.raises(ValueError):
    reaper_mod._reap_with_cluster(MagicMock(), "", delete_pods=False)
  with pytest.raises(SystemExit):
    reaper_mod.main(["--run-id", "", "--namespace", "ns"])


def test_run_scoped_reap_keeps_pools_another_runs_claims_use():
  c = _linked_cluster()
  base = c.resources._list_objects.side_effect
  def _list(plural, sel=None, **kw):
    if plural == "sandboxclaims" and sel is None:    # every claim in the namespace
      return [{"metadata": {"name": "theirs", "labels": {constants.RUN_ID_LABEL: "live"}},
               "spec": {"warmPoolRef": {"name": "pool-a"}}},
              {"metadata": {"name": "ours", "labels": {constants.RUN_ID_LABEL: "r"}},
               "spec": {"warmPoolRef": {"name": "pool-b"}}}]
    if plural == "sandboxwarmpools" and sel is None:
      return [{"metadata": {"name": "pool-a"}, "spec": {"sandboxTemplateRef": {"name": "tpl-1"}}}]
    return base(plural, sel, **kw)
  c.resources._list_objects.side_effect = _list
  counts = reaper_mod._reap_with_cluster(c, "r", delete_pods=True,
                                         protect_claims_of=lambda rid: rid != "r")
  assert [k.args[0] for k in c.resources.delete_warmpool.call_args_list] == ["pool-b"]
  c.resources.delete_template.assert_not_called()                  # tpl-1 backs pool-a
  swept = [k.kwargs["label_selector"] for k in c.core_api.delete_collection_namespaced_pod.call_args_list]
  assert "agents.x-k8s.io/warm-pool-sandbox=aaaa" not in swept     # pool-a's pods stay
  deleted_sb = {k.args[0] for k in c.resources.delete_sandbox.call_args_list}
  assert "sb-pool-a" not in deleted_sb
  assert counts["kept_in_use"] == 2


def _lhandle(pod, uid="u-1", restarts=0):
  core = MagicMock()
  core.read_namespaced_pod_status.return_value = pod
  h = _handle(core)
  h.pod_uid, h.runtime_restarts = uid, restarts
  return h


def _running(uid="u-1", restarts=0, name="agent-runtime", extra=()):
  return {"metadata": {"name": "p", "uid": uid},
          "status": {"phase": "Running", "container_statuses": [
              {"name": name, "restart_count": restarts, "state": {"running": {}}}, *extra]}}


def test_is_alive_detects_a_same_name_replacement_pod():
  assert _lhandle(_running("u-1")).is_alive() is True
  assert _lhandle(_running("u-2")).is_alive() is False


def test_is_alive_detects_an_in_place_restart():
  assert _lhandle(_running(restarts=1), restarts=0).is_alive() is False


def test_is_alive_ignores_sidecars_when_the_runtime_container_is_unknown():
  sidecar_done = {"name": "log-shipper", "state": {"terminated": {"reason": "Completed"}}}
  pod = _running(name="something-else", extra=[sidecar_done])
  assert _lhandle(pod, uid=None, restarts=None).is_alive() is True


def test_record_pod_identity_at_acquire_and_best_effort():
  core = MagicMock()
  core.read_namespaced_pod_status.return_value = _running("u-7", restarts=2)
  h = _handle(core)
  h.record_pod_identity()
  assert (h.pod_uid, h.runtime_restarts) == ("u-7", 2)
  core.read_namespaced_pod_status.side_effect = client.ApiException(status=500)
  h2 = _handle(core)
  h2.record_pod_identity()                       # never raises
  assert (h2.pod_uid, h2.runtime_restarts) == (None, None)

