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
"""Concurrent runs sharing a cluster (kubernetes-sigs/agent-sandbox#1736):
run-scoped teardown, fail-fast on a pool deleted mid-wait, the ownership guard
on by-name pool writes, and the two `run_isolation` flavours — per-run names in
a shared namespace, or a per-run namespace."""
import time
from unittest.mock import MagicMock

import pytest
from kubernetes import client

from agent_sandbox_rl import (ClusterConfig, ClusterRegistry, FleetConfig,
                              SandboxFleet, constants)
from agent_sandbox_rl.config import run_namespace
from agent_sandbox_rl.exceptions import FleetError
from agent_sandbox_rl.resources import Resources

IMG = "registry.example/repo/img:tag"


def _fleet(c, **cfg):
  return SandboxFleet(FleetConfig(**cfg), registry=ClusterRegistry([c]))


def _labelled(run_id):
  return {"metadata": {"labels": {constants.RUN_ID_LABEL: run_id}}}


def _resources():
  return Resources(MagicMock(), MagicMock(), "ns")


# --- teardown is scoped to this run ---------------------------------------- #
def test_teardown_lists_by_run_selector_not_the_managed_label(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.teardown()
  sel = f.run_selector()
  assert sel == f"{constants.RUN_ID_LABEL}={f.run_id}"
  c.resources.list_claims.assert_called_once_with(label_selector=sel)
  c.resources.list_warmpools.assert_called_once_with(label_selector=sel)
  c.resources.list_templates.assert_called_once_with(label_selector=sel)
  c.resources.managed_selector.assert_not_called()


def test_explicit_delete_namespace_is_still_honoured(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.teardown(delete_namespace=True)
  c.resources.delete_namespace.assert_called_once_with("ns")


# --- run_isolation="names" ------------------------------------------------- #
def test_names_mode_bakes_the_run_id_into_template_and_pool_names(make_cluster):
  cfg = FleetConfig(run_isolation="names", template_name_prefix="oh-img-")
  f1 = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("a")]))
  f2 = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("b")]))
  h = cfg.image_hash(IMG)
  t1, t2 = f1.config.template_name(IMG), f2.config.template_name(IMG)
  assert t1 == f"oh-img-{f1.run_id}-{h}"
  assert t2 == f"oh-img-{f2.run_id}-{h}"
  assert t1 != t2
  assert f1.config.pool_name(IMG) == f"pool-{t1}"     # pools derive from templates
  assert cfg.template_name_prefix == "oh-img-"        # caller's config untouched


def test_none_mode_keeps_the_historical_names(make_cluster):
  cfg = FleetConfig(template_name_prefix="oh-img-")
  f = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("a")]))
  assert f.config.template_name(IMG) == cfg.template_name(IMG)
  assert f.config.pool_name(IMG) == cfg.pool_name(IMG)
  assert f.run_id not in f.config.pool_name(IMG)


def test_run_id_placeholder_is_substituted_in_any_mode(make_cluster):
  cfg = FleetConfig(template_name_prefix="oh-{run_id}-img-",
                    pool_name_format="p-{run_id}-{image_hash}")
  f = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("a")]))
  h = cfg.image_hash(IMG)
  assert f.config.template_name(IMG) == f"oh-{f.run_id}-img-{h}"
  assert f.config.pool_name(IMG) == f"p-{f.run_id}-{h}"


def test_names_mode_does_not_double_up_an_explicit_placeholder(make_cluster):
  cfg = FleetConfig(run_isolation="names", template_name_prefix="x-{run_id}-")
  f = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("a")]))
  assert f.config.template_name_prefix == f"x-{f.run_id}-"


def test_unknown_isolation_mode_is_rejected():
  with pytest.raises(ValueError, match="run_isolation"):
    FleetConfig(run_isolation="pods")


def test_run_namespace_must_be_a_dns_label():
  assert run_namespace("trellis", "0123456789ab") == "trellis-0123456789ab"
  with pytest.raises(ValueError):
    run_namespace("a" * 55, "0123456789ab")      # 68 chars > 63
  with pytest.raises(ValueError):
    run_namespace("Trellis", "0123456789ab")     # uppercase


# --- run_isolation="namespace" --------------------------------------------- #
def test_namespace_mode_creates_owns_and_deletes_the_run_namespace(make_cluster):
  c = make_cluster("solo", namespace="rl")
  c.resources.ensure_namespace.return_value = True
  hook_calls = []
  cfg = FleetConfig(run_isolation="namespace",
                    clusters=[ClusterConfig(namespace="rl")],
                    run_namespace_labels={"kueue.x-k8s.io/queue-name": "q"},
                    run_namespace_setup=lambda cl, ns: hook_calls.append((cl.name, ns)))
  f = SandboxFleet(cfg, registry=ClusterRegistry([c]))
  ns = f"rl-{f.run_id}"
  assert f.config.clusters[0].namespace == ns              # resolved config
  assert c.namespace == ns and c.resources.namespace == ns  # explicit registry re-pointed
  assert cfg.clusters[0].namespace == "rl"                 # caller's config untouched
  c.resources.ensure_namespace.assert_not_called()         # nothing created at construction

  f.load_tasks([IMG])
  f.plan()
  c.resources.ensure_namespace.assert_called_once()
  args, kw = c.resources.ensure_namespace.call_args
  assert args[0] == ns
  assert kw["labels"][constants.RUN_ID_LABEL] == f.run_id
  assert kw["labels"]["kueue.x-k8s.io/queue-name"] == "q"
  assert hook_calls == [("solo", ns)]
  f.plan()                                                 # idempotent
  c.resources.ensure_namespace.assert_called_once()

  f.teardown()
  c.resources.delete_namespace.assert_called_once_with(ns)


def test_namespace_mode_through_the_default_registry(monkeypatch):
  # Real callers build the registry from config.clusters: the per-run namespace
  # must already be in the ClusterConfig when the Cluster is constructed, and the
  # explicit-registry re-pointing must leave those clusters alone.
  import agent_sandbox_rl.cluster as cl
  monkeypatch.setattr(cl, "build_api_client", lambda cfg: object())
  f = SandboxFleet(FleetConfig(run_isolation="namespace",
                               clusters=[ClusterConfig(name="c1", namespace="rl"),
                                         ClusterConfig(name="c2", namespace="other")]))
  for name, base in (("c1", "rl"), ("c2", "other")):
    c = f.registry.get(name)
    assert c.namespace == f"{base}-{f.run_id}"
    assert c.resources.namespace == c.namespace


def test_adopt_existing_cannot_be_combined_with_a_per_run_namespace():
  with pytest.raises(ValueError, match="adopt_existing"):
    FleetConfig(adopt_existing=True, run_isolation="namespace")
  FleetConfig(adopt_existing=True, run_isolation="names")    # discovery is by image: fine


def test_namespace_mode_leaves_a_preexisting_namespace_alone(make_cluster):
  c = make_cluster("solo", namespace="rl")
  c.resources.ensure_namespace.return_value = False        # 409: someone else's
  f = _fleet(c, run_isolation="namespace")
  f.load_tasks([IMG])
  f.plan()
  f.teardown()
  c.resources.delete_namespace.assert_not_called()


def test_namespace_mode_create_failure_is_actionable(make_cluster):
  c = make_cluster("solo", namespace="rl")
  c.resources.ensure_namespace.side_effect = client.ApiException(status=403)
  f = _fleet(c, run_isolation="namespace")
  f.load_tasks([IMG])
  with pytest.raises(FleetError, match="run_isolation='names'"):
    f.plan()


# --- ownership guard on by-name pool writes -------------------------------- #
def test_unwarm_leaves_a_pool_owned_by_another_run(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  c.resources.get_warmpool.return_value = _labelled("other-run-0001")
  f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_not_called()
  c.resources.delete_template.assert_not_called()


def test_unwarm_deletes_a_pool_this_run_owns(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  c.resources.get_warmpool.return_value = _labelled(f.run_id)
  f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_called_once_with(f.config.pool_name(IMG))


def test_set_pool_replicas_refuses_to_resize_another_runs_pool(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  before = c.active_replicas
  c.resources.create_warmpool.return_value = False      # 409 owner check: theirs
  f.set_pool_replicas(IMG, 5)
  _, kw = c.resources.create_warmpool.call_args
  assert kw["owner_run_id"] == f.run_id and kw["reconcile"] is True
  assert c.active_replicas == before                    # nothing reserved for it
  assert f._warmed[IMG] != 5


def test_set_pool_replicas_does_not_drop_a_scale_change_on_a_read_error(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  c.resources.get_warmpool.side_effect = RuntimeError("apiserver hiccup")
  f.set_pool_replicas(IMG, 0)                           # no pre-read to fail on
  assert f._warmed[IMG] == 0
  c.resources.create_warmpool.side_effect = RuntimeError("apiserver hiccup")
  with pytest.raises(RuntimeError):                     # surfaced, not swallowed
    f.set_pool_replicas(IMG, 1)


# --- wait_for_pool_ready fails fast when the pool disappears --------------- #
def test_wait_for_pool_ready_gives_up_on_a_deleted_event(monkeypatch, caplog):
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 0}}

  class FakeWatch:
    def stream(self, func, **kw):
      return [{"type": "DELETED", "object": {"metadata": {"name": "pool-x"},
                                             "status": {"readyReplicas": 0}}}]

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: FakeWatch())
  t0 = time.monotonic()
  with caplog.at_level("ERROR", logger="agent_sandbox_rl.resources"):
    assert r.wait_for_pool_ready("pool-x", 2, timeout=30) is False
  assert time.monotonic() - t0 < 1.0                       # not the 30 s timeout
  assert "deleted" in caplog.text


def test_wait_for_pool_ready_gives_up_when_the_pool_is_gone_after_a_watch_drop(monkeypatch):
  r = _resources()
  # Fast path sees the pool (not ready); the watch drops; the re-check 404s.
  r.custom_api.get_namespaced_custom_object.side_effect = [
      {"status": {"readyReplicas": 0}}, client.ApiException(status=404)]

  class FakeWatch:
    def stream(self, func, **kw):
      raise RuntimeError("connection reset")

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: FakeWatch())
  t0 = time.monotonic()
  assert r.wait_for_pool_ready("pool-x", 2, timeout=30, poll_interval=0.01) is False
  assert time.monotonic() - t0 < 1.0


# --- Resources helpers ------------------------------------------------------ #
def test_get_warmpool_is_none_when_missing():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  assert r.get_warmpool("nope") is None


def test_ensure_namespace_reports_created_vs_existing():
  r = _resources()
  assert r.ensure_namespace("rl-abc", labels={"a": "b"}) is True
  body = r.core_api.create_namespace.call_args[0][0]
  assert body.metadata.name == "rl-abc" and body.metadata.labels == {"a": "b"}
  r.core_api.create_namespace.side_effect = client.ApiException(status=409)
  assert r.ensure_namespace("rl-abc") is False
  r.core_api.create_namespace.side_effect = client.ApiException(status=403)
  with pytest.raises(client.ApiException):
    r.ensure_namespace("rl-abc")


def test_delete_namespace_tolerates_404():
  r = _resources()
  r.core_api.delete_namespace.side_effect = client.ApiException(status=404)
  r.delete_namespace("gone")                               # no raise


# --- review follow-ups (#1737) --------------------------------------------- #
def test_pool_name_format_with_only_a_run_id_part_is_rejected():
  # `{run_id}` must not satisfy the per-image uniqueness check: such a format
  # would map every image onto one pool.
  with pytest.raises(ValueError, match="same pool name"):
    FleetConfig(pool_name_format="pool-{run_id}")


def test_names_mode_scopes_template_and_pool_independently(make_cluster):
  # A placeholder in only the pool format must not leave templates shared.
  cfg = FleetConfig(run_isolation="names", pool_name_format="p-{run_id}-{image_hash}")
  f = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("a")]))
  assert f.run_id in f.config.template_name(IMG)
  assert f.run_id in f.config.pool_name(IMG)
  # ...and a placeholder in only the prefix must not leave pools shared when the
  # pool format carries neither {template} nor {run_id}.
  cfg = FleetConfig(run_isolation="names", template_name_prefix="x-{run_id}-",
                    pool_name_format="pool-{image_hash}")
  f = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("b")]))
  assert f.config.pool_name(IMG) == f"pool-{cfg.image_hash(IMG)}-{f.run_id}"
  # {template} already inherits the prefix's run id: the format is left alone.
  cfg = FleetConfig(run_isolation="names", pool_name_format="{template}-pool")
  f = SandboxFleet(cfg, registry=ClusterRegistry([make_cluster("c")]))
  assert f.config.pool_name(IMG) == f"{f.config.template_name(IMG)}-pool"


def test_unwarm_keeps_the_reservation_when_ownership_cannot_be_read(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  reserved = c.active_replicas
  assert reserved > 0
  c.resources.get_warmpool.side_effect = RuntimeError("apiserver hiccup")
  with pytest.raises(RuntimeError, match="hiccup"):
    f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_not_called()
  assert IMG in f._warmed and c.active_replicas == reserved   # nothing leaked
  c.resources.get_warmpool.side_effect = None                 # blip over: retry
  f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_called_once()
  assert IMG not in f._warmed and c.active_replicas == 0      # released exactly once


def test_warm_refuses_a_pool_name_taken_by_another_run(make_cluster):
  c = make_cluster("solo")
  c.resources.create_warmpool.return_value = False       # 409 owner check: theirs
  c.resources.get_warmpool.return_value = _labelled("other-run-0001")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  with pytest.raises(FleetError) as ei:
    f.warm_image(IMG, wait=True)
  msg = str(ei.value)
  assert "other-run-0001" in msg and f.config.pool_name(IMG) in msg
  assert "run_isolation='names'" in msg and "adopt_existing=True" in msg
  assert "--run-id other-run-0001" in msg                # a runnable reap hint
  _, kw = c.resources.ensure_template.call_args
  assert kw["owner_run_id"] == f.run_id                  # no relabel of their template
  c.resources.wait_for_pool_ready.assert_not_called()   # no borrowing, no stall
  assert IMG not in f._warmed and c.active_replicas == 0
  f.unwarm_image(IMG)                                    # nothing recorded: a no-op
  c.resources.delete_warmpool.assert_not_called()


def test_warm_reuse_does_not_read_the_pool(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  f.warm_image(IMG, wait=False)                          # cross-epoch reuse
  c.resources.get_warmpool.assert_not_called()
  assert c.resources.create_warmpool.call_count == 1


def test_wait_for_pool_ready_gives_up_when_the_pool_is_already_gone(monkeypatch):
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)

  # Record rather than raise: an exception from stream() would be swallowed by
  # the dropped-watch handler and the re-check would still return False, which
  # proves nothing about the fast path.
  class RecordingWatch:
    opened = False

    def stream(self, func, **kw):
      RecordingWatch.opened = True
      return []

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: RecordingWatch())
  assert r.wait_for_pool_ready("pool-x", 2, timeout=30) is False
  assert RecordingWatch.opened is False                    # decided on the initial read
  assert r.custom_api.get_namespaced_custom_object.call_count == 1


def test_wait_for_pool_ready_gives_up_when_the_pool_is_gone_after_an_api_error(monkeypatch):
  # The ApiException branch (a transient 500 from the watch) re-checks too.
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = [
      {"status": {"readyReplicas": 0}}, client.ApiException(status=404)]

  class FlakyWatch:
    def stream(self, func, **kw):
      raise client.ApiException(status=500)

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: FlakyWatch())
  t0 = time.monotonic()
  assert r.wait_for_pool_ready("pool-x", 2, timeout=30, poll_interval=0.01) is False
  assert time.monotonic() - t0 < 1.0


def test_namespace_mode_rolls_back_namespaces_created_before_a_failure(make_cluster):
  a, b = make_cluster("a", namespace="rl"), make_cluster("b", namespace="rl")
  a.resources.ensure_namespace.return_value = True
  b.resources.ensure_namespace.side_effect = client.ApiException(status=403)
  f = SandboxFleet(FleetConfig(run_isolation="namespace"), registry=ClusterRegistry([a, b]))
  f.load_tasks([IMG])
  with pytest.raises(FleetError):
    f.plan()
  a.resources.delete_namespace.assert_called_once_with(a.namespace)   # rolled back
  assert f._created_namespaces == set() and f._namespaces_ensured is False


def test_namespace_mode_setup_hook_failure_rolls_back_and_retries(make_cluster):
  c = make_cluster("solo", namespace="rl")
  c.resources.ensure_namespace.return_value = True     # created (again) on each attempt
  attempts = []

  def hook(cluster, ns):
    attempts.append(ns)
    if len(attempts) == 1:
      raise RuntimeError("LocalQueue create failed")

  f = _fleet(c, run_isolation="namespace", run_namespace_setup=hook)
  f.load_tasks([IMG])
  with pytest.raises(FleetError, match="run_namespace_setup"):
    f.plan()
  c.resources.delete_namespace.assert_called_once_with(c.namespace)   # rolled back
  assert f._namespaces_ensured is False
  f.plan()                                             # retry re-creates and re-runs the hook
  assert attempts == [c.namespace, c.namespace]
  assert f._namespaces_ensured is True
  assert (c.name, c.namespace) in f._created_namespaces


def test_ensure_namespace_rejects_a_terminating_namespace():
  import types
  r = _resources()
  r.core_api.create_namespace.side_effect = client.ApiException(status=409)
  r.core_api.read_namespace.return_value = types.SimpleNamespace(
      status=types.SimpleNamespace(phase="Terminating"))
  with pytest.raises(RuntimeError, match="terminating"):
    r.ensure_namespace("rl-abc")
  r.core_api.read_namespace.return_value = types.SimpleNamespace(
      status=types.SimpleNamespace(phase="Active"))
  assert r.ensure_namespace("rl-abc") is False


# --- review round 2: atomic writes, length after injection, rollback ---------- #
def test_names_mode_rejects_a_prefix_that_overflows_after_injection(make_cluster):
  prefix = "a" * 235 + "-"          # template 236+12 = 248, pool "pool-"+248 = 253: valid alone
  cfg = FleetConfig(template_name_prefix=prefix)
  assert len(cfg.template_name(IMG)) == 248 and len(cfg.pool_name(IMG)) == 253
  with pytest.raises(ValueError, match="after the run id"):
    SandboxFleet(FleetConfig(run_isolation="names", template_name_prefix=prefix),
                 registry=ClusterRegistry([make_cluster("a")]))
  # and via the pool format alone
  fmt = "p" * 235 + "-{image_hash}"                     # 236 + 12 = 248: valid alone
  with pytest.raises(ValueError, match="after the run id"):
    SandboxFleet(FleetConfig(run_isolation="names", pool_name_format=fmt),
                 registry=ClusterRegistry([make_cluster("b")]))


def test_delete_warmpool_uses_a_uid_precondition_and_tolerates_replacement():
  r = _resources()
  r.delete_warmpool("pool-x", uid="uid-1")
  _, kw = r.custom_api.delete_namespaced_custom_object.call_args
  assert kw["body"].preconditions.uid == "uid-1"
  r.custom_api.delete_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.delete_warmpool("pool-x", uid="uid-1")               # replaced meanwhile: no raise
  with pytest.raises(client.ApiException):
    r.delete_warmpool("pool-x")                          # a 409 without a precondition is real


def test_create_warmpool_refuses_to_resize_another_runs_pool_at_the_409():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": {constants.RUN_ID_LABEL: "other"}, "resourceVersion": "7"}}
  assert r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine") is False
  r.custom_api.patch_namespaced_custom_object.assert_not_called()


def test_create_warmpool_reconciles_with_an_optimistic_lock():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": {constants.RUN_ID_LABEL: "mine"}, "resourceVersion": "7"}}
  assert r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine") is True
  _, kw = r.custom_api.patch_namespaced_custom_object.call_args
  assert kw["body"] == {"spec": {"replicas": 3}, "metadata": {"resourceVersion": "7"}}


def test_create_warmpool_reinspects_after_a_conflicting_update():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.custom_api.get_namespaced_custom_object.side_effect = [
      {"metadata": {"labels": {constants.RUN_ID_LABEL: "mine"}, "resourceVersion": "7"}},
      {"metadata": {"labels": {constants.RUN_ID_LABEL: "mine"}, "resourceVersion": "8"}}]
  # a status write by the controller between the read and the patch
  r.custom_api.patch_namespaced_custom_object.side_effect = [
      client.ApiException(status=409), None]
  assert r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine") is True
  _, kw = r.custom_api.patch_namespaced_custom_object.call_args
  assert kw["body"]["metadata"] == {"resourceVersion": "8"}
  # relabelled by another run in between: the re-read's owner check stops it
  r.custom_api.get_namespaced_custom_object.side_effect = [
      {"metadata": {"labels": {constants.RUN_ID_LABEL: "mine"}, "resourceVersion": "7"}},
      {"metadata": {"labels": {constants.RUN_ID_LABEL: "other"}, "resourceVersion": "9"}}]
  r.custom_api.patch_namespaced_custom_object.reset_mock()
  r.custom_api.patch_namespaced_custom_object.side_effect = [client.ApiException(status=409)]
  assert r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine") is False
  assert r.custom_api.patch_namespaced_custom_object.call_count == 1


def test_create_warmpool_gives_up_if_the_pool_keeps_changing():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": {constants.RUN_ID_LABEL: "mine"}, "resourceVersion": "7"}}
  r.custom_api.patch_namespaced_custom_object.side_effect = client.ApiException(status=409)
  with pytest.raises(RuntimeError, match="kept changing"):
    r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine")


def test_create_warmpool_owner_checks_a_pool_recreated_in_the_race():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.custom_api.get_namespaced_custom_object.side_effect = [
      client.ApiException(status=404),                   # vanished after the 409 ...
      {"metadata": {"labels": {constants.RUN_ID_LABEL: "other"}, "resourceVersion": "3"}}]
  # ... and a concurrent run re-created it first: its pool, so False, not a raw 409
  assert r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine") is False
  assert r.custom_api.create_namespaced_custom_object.call_count == 2
  r.custom_api.patch_namespaced_custom_object.assert_not_called()


def test_create_warmpool_recreates_when_the_pool_vanished_after_the_409():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = [
      client.ApiException(status=409), None]
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  assert r.create_warmpool("pool-x", "tmpl", 3, reconcile=True, owner_run_id="mine") is True
  assert r.custom_api.create_namespaced_custom_object.call_count == 2


def test_ensure_template_refuses_another_runs_template():
  from agent_sandbox_rl import TemplateSpec
  from agent_sandbox_rl.exceptions import OwnedByAnotherRunError
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": {constants.RUN_ID_LABEL: "other"}},
      "spec": {"podTemplate": {"metadata": {"labels": {}}}}}
  with pytest.raises(OwnedByAnotherRunError) as ei:
    r.ensure_template(IMG, "tmpl", TemplateSpec(), owner_run_id="mine")
  assert ei.value.owner == "other"
  r.custom_api.patch_namespaced_custom_object.assert_not_called()   # no relabel
  # Without owner_run_id (the bulk / on-demand callers) it is still "existed".
  r.custom_api.get_namespaced_custom_object.return_value = {
      "metadata": {"labels": {constants.RUN_ID_LABEL: "mine"}},
      "spec": {"podTemplate": {"metadata": {"labels": {}}}}}
  assert r.ensure_template(IMG, "tmpl", TemplateSpec()) is False


def test_ensure_template_owner_checks_a_template_created_in_the_race():
  from agent_sandbox_rl import TemplateSpec
  from agent_sandbox_rl.exceptions import OwnedByAnotherRunError
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = [
      client.ApiException(status=404),                 # absent at the get ...
      {"metadata": {"labels": {constants.RUN_ID_LABEL: "other"}}}]
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(
      status=409)                                      # ... another run won the create
  with pytest.raises(OwnedByAnotherRunError):
    r.ensure_template(IMG, "tmpl", TemplateSpec(), owner_run_id="mine")


def test_unwarm_deletes_exactly_the_inspected_pool(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  c.resources.get_warmpool.return_value = {
      "metadata": {"uid": "uid-42", "labels": {constants.RUN_ID_LABEL: f.run_id}}}
  f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_called_once_with(f.config.pool_name(IMG), uid="uid-42")


def test_rollback_keeps_ownership_when_the_namespace_delete_fails(make_cluster):
  c = make_cluster("solo", namespace="rl")
  c.resources.ensure_namespace.return_value = True
  c.resources.delete_namespace.side_effect = [RuntimeError("apiserver"), None]

  def hook(cluster, ns):
    raise RuntimeError("queue create failed")

  f = _fleet(c, run_isolation="namespace", run_namespace_setup=hook)
  f.load_tasks([IMG])
  with pytest.raises(FleetError):
    f.plan()
  assert (c.name, c.namespace) in f._created_namespaces  # rollback failed: still ours
  f.teardown()                                           # ordinary teardown retries it
  assert c.resources.delete_namespace.call_count == 2


# --- review round 3: tri-state ownership, no blind deletes, namespace state -- #
def test_unwarm_of_a_pool_already_gone_issues_no_delete_by_name(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  c.resources.get_warmpool.return_value = None           # deleted since it was warmed
  f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_not_called()        # could hit a re-created pool
  c.resources.delete_template.assert_called_once_with(f.config.template_name(IMG))
  assert IMG not in f._warmed and c.active_replicas == 0


def test_teardown_keeps_namespace_ownership_when_its_delete_fails(make_cluster):
  c = make_cluster("solo", namespace="rl")
  c.resources.ensure_namespace.return_value = True
  c.resources.delete_namespace.side_effect = [RuntimeError("apiserver"), None]
  f = _fleet(c, run_isolation="namespace")
  f.load_tasks([IMG])
  f.plan()
  f.teardown()
  assert (c.name, c.namespace) in f._created_namespaces  # still ours: retry next cycle
  f._torndown = False                                    # what setup() does per cycle
  f.teardown()
  assert c.resources.delete_namespace.call_count == 2
  assert f._created_namespaces == set()


def test_setup_hook_reruns_on_a_namespace_kept_after_a_failed_rollback(make_cluster):
  c = make_cluster("solo", namespace="rl")
  created = iter([True])                                    # then "already exists"
  c.resources.ensure_namespace.side_effect = lambda *a, **k: next(created, False)
  c.resources.delete_namespace.side_effect = RuntimeError("apiserver")
  attempts = []

  def hook(cluster, ns):
    attempts.append(ns)
    if len(attempts) == 1:
      raise RuntimeError("LocalQueue create failed")

  f = _fleet(c, run_isolation="namespace", run_namespace_setup=hook)
  f.load_tasks([IMG])
  with pytest.raises(FleetError, match="run_namespace_setup"):
    f.plan()
  assert (c.name, c.namespace) in f._created_namespaces  # rollback delete failed
  f.plan()                                               # exists now, but not set up
  assert attempts == [c.namespace, c.namespace]
  assert f._namespaces_ensured is True
  f.plan_ = None
  f._namespaces_ensured = False
  f.plan()                                               # set up once: not re-run
  assert len(attempts) == 2


# --- review round 4: the template side of a collision ----------------------- #
def test_warm_refuses_to_build_on_another_runs_template(make_cluster):
  from agent_sandbox_rl.exceptions import OwnedByAnotherRunError
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  # Their pool is gone but their template remains: no 409 would ever fire.
  c.resources.ensure_template.side_effect = OwnedByAnotherRunError(
      "SandboxTemplate", f.config.template_name(IMG), "other-run-0001")
  with pytest.raises(FleetError) as ei:
    f.warm_image(IMG, wait=True)
  msg = str(ei.value)
  assert "template" in msg and "other-run-0001" in msg
  assert "--run-id other-run-0001" in msg
  c.resources.create_warmpool.assert_not_called()        # nothing built on theirs
  assert IMG not in f._warmed and c.active_replicas == 0


def test_unwarm_leaves_another_runs_template_and_deletes_ours_by_uid(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  tmpl = f.config.template_name(IMG)
  f.warm_image(IMG, wait=False)
  c.resources.get_template.return_value = _labelled("other-run-0001")
  f.unwarm_image(IMG)
  c.resources.delete_warmpool.assert_called_once()       # our pool still goes
  c.resources.delete_template.assert_not_called()        # their template stays
  f.warm_image(IMG, wait=False)
  c.resources.get_template.return_value = {
      "metadata": {"uid": "t-7", "labels": {constants.RUN_ID_LABEL: f.run_id}}}
  f.unwarm_image(IMG)
  c.resources.delete_template.assert_called_once_with(tmpl, uid="t-7")


def test_unwarm_issues_no_template_delete_by_name_when_it_is_gone(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.load_tasks([IMG])
  f.plan()
  f.warm_image(IMG, wait=False)
  c.resources.get_template.return_value = None
  f.unwarm_image(IMG)
  c.resources.delete_template.assert_not_called()


def test_on_demand_rollback_leaves_another_runs_pool_and_template(make_cluster):
  c = make_cluster("solo")
  f = _fleet(c)
  f.plan()                                               # empty plan: on-demand
  c.resources.get_warmpool.return_value = _labelled("other-run-0001")
  c.resources.get_template.return_value = _labelled("other-run-0001")
  c.sandbox_client.create_sandbox.side_effect = RuntimeError("claim failed")
  with pytest.raises(RuntimeError, match="claim failed"):
    f.acquire(f.load_tasks([IMG])[0])
  c.resources.delete_warmpool.assert_not_called()
  c.resources.delete_template.assert_not_called()


def test_template_helpers():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  assert r.get_template("nope") is None
  r.delete_template("tmpl", uid="u-1")
  _, kw = r.custom_api.delete_namespaced_custom_object.call_args
  assert kw["body"].preconditions.uid == "u-1"

