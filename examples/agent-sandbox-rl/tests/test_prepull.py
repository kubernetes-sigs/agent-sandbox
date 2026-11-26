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

import sys
import types

import pytest
from unittest.mock import MagicMock

from kubernetes import client

from agent_sandbox_rl import prepull, prepull_delete


def _cluster():
  return types.SimpleNamespace(name="c", namespace="ns", apps_api=MagicMock())


def _no_sleep(monkeypatch, *, monotonic=None):
  # The package re-exports prepull() under the module's name, so patch the
  # module object's `time` rather than a dotted path.
  import time as _time
  mod = sys.modules["agent_sandbox_rl.prepull"]
  monkeypatch.setattr(mod, "time", types.SimpleNamespace(
      monotonic=monotonic or _time.monotonic, sleep=lambda s: None))


def _status(desired, ready):
  return types.SimpleNamespace(status=types.SimpleNamespace(
      desired_number_scheduled=desired, number_ready=ready))


def test_manifest_dedupes_and_waits():
  c = _cluster()
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(3, 3)
  ok = prepull(c, ["imgA", "imgB", "imgA"], node_selector={"k": "v"},
               image_pull_secret="ps", wait=True, timeout=5)
  assert ok is True
  ns_arg, body = c.apps_api.create_namespaced_daemon_set.call_args.args
  assert ns_arg == "ns"
  pod = body["spec"]["template"]["spec"]
  assert [ic["image"] for ic in pod["initContainers"]] == ["imgA", "imgB"]  # de-duped
  assert pod["nodeSelector"] == {"k": "v"}
  assert pod["imagePullSecrets"] == [{"name": "ps"}]
  assert pod["containers"][0]["name"] == "pause"


def test_no_wait_skips_status():
  c = _cluster()
  assert prepull(c, ["x"], wait=False) is True
  c.apps_api.read_namespaced_daemon_set_status.assert_not_called()


def test_timeout_returns_false():
  c = _cluster()
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(3, 0)
  assert prepull(c, ["x"], wait=True, timeout=0) is False


def test_empty_images_noop():
  c = _cluster()
  assert prepull(c, []) is True
  c.apps_api.create_namespaced_daemon_set.assert_not_called()


def test_existing_ds_with_the_same_selector_is_replaced():
  c = _cluster()
  c.apps_api.create_namespaced_daemon_set.side_effect = client.ApiException(status=409)
  c.apps_api.read_namespaced_daemon_set.return_value = types.SimpleNamespace(
      spec=types.SimpleNamespace(selector=types.SimpleNamespace(
          match_labels={"app": "agent-sandbox-rl", "role": "prepull",
                        "prepull-ds": "agent-sandbox-rl-prepull"})))
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(1, 1)
  assert prepull(c, ["x"], wait=True, timeout=5) is True
  c.apps_api.replace_namespaced_daemon_set.assert_called_once()
  c.apps_api.patch_namespaced_daemon_set.assert_not_called()
  c.apps_api.delete_namespaced_daemon_set.assert_not_called()


def test_existing_ds_with_another_selector_is_recreated(monkeypatch):
  _no_sleep(monkeypatch)
  c = _cluster()
  c.apps_api.create_namespaced_daemon_set.side_effect = [
      client.ApiException(status=409), client.ApiException(status=409), None]
  c.apps_api.read_namespaced_daemon_set.return_value = types.SimpleNamespace(
      spec=types.SimpleNamespace(selector=types.SimpleNamespace(
          match_labels={"app": "agent-sandbox-rl", "role": "prepull",
                        "agents.x-k8s.io/asrl-run-id": "old-run"})))
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(1, 1)
  assert prepull(c, ["x"], wait=True, timeout=5) is True
  c.apps_api.delete_namespaced_daemon_set.assert_called_once()
  assert c.apps_api.create_namespaced_daemon_set.call_count == 3    # 409, deleted, 409, ok
  c.apps_api.replace_namespaced_daemon_set.assert_not_called()


def test_selector_is_run_independent_and_labels_carry_the_run():
  c = _cluster()
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(1, 1)
  labels = {"app": "agent-sandbox-rl", "agents.x-k8s.io/asrl-run-id": "run-1"}
  prepull(c, ["x"], labels=labels, runtime_class="gvisor", wait=True, timeout=5)
  _, body = c.apps_api.create_namespaced_daemon_set.call_args.args
  sel = body["spec"]["selector"]["matchLabels"]
  assert sel == {"app": "agent-sandbox-rl", "role": "prepull", "prepull-ds": "agent-sandbox-rl-prepull"}
  assert body["metadata"]["labels"]["agents.x-k8s.io/asrl-run-id"] == "run-1"
  tmpl = body["spec"]["template"]
  assert tmpl["metadata"]["labels"]["agents.x-k8s.io/asrl-run-id"] == "run-1"
  assert all(tmpl["metadata"]["labels"][k] == v for k, v in sel.items())
  assert tmpl["spec"]["runtimeClassName"] == "gvisor"
  with pytest.raises(ValueError, match="63"):
    prepull(c, ["x"], ds_name="p" * 64, wait=False)


def test_readiness_needs_the_current_generation_and_an_updated_rollout():
  c = _cluster()
  stale = types.SimpleNamespace(
      metadata=types.SimpleNamespace(generation=2),
      status=types.SimpleNamespace(desired_number_scheduled=2, number_ready=2,
                                   observed_generation=1, updated_number_scheduled=0))
  fresh = types.SimpleNamespace(
      metadata=types.SimpleNamespace(generation=2),
      status=types.SimpleNamespace(desired_number_scheduled=2, number_ready=2,
                                   observed_generation=2, updated_number_scheduled=2))
  c.apps_api.read_namespaced_daemon_set_status.side_effect = [stale, stale, fresh]
  assert prepull(c, ["x"], wait=True, timeout=30, poll_interval=0) is True
  assert c.apps_api.read_namespaced_daemon_set_status.call_count == 3


def test_delete_swallows_404():
  c = _cluster()
  c.apps_api.delete_namespaced_daemon_set.side_effect = client.ApiException(status=404)
  prepull_delete(c)  # no raise


# ------------------------------------------------- batching, pacing, cleanup


class _Tracker:
  """apps_api stand-in that records DaemonSet lifecycle and the peak number alive."""

  def __init__(self, ready=True):
    self.created, self.deleted, self.alive, self.peak = [], [], set(), 0
    self.ready = ready
    self.apps_api = MagicMock()
    self.apps_api.create_namespaced_daemon_set.side_effect = self._create
    self.apps_api.delete_namespaced_daemon_set.side_effect = self._delete
    self.apps_api.read_namespaced_daemon_set_status.side_effect = (
        lambda name, ns: _status(2, 2 if self.ready else 0))
    self.name, self.namespace = "c", "ns"

  def _create(self, ns, body):
    name = body["metadata"]["name"]
    self.created.append(body)
    self.alive.add(name)
    self.peak = max(self.peak, len(self.alive))

  def _delete(self, name, ns):
    self.deleted.append(name)
    self.alive.discard(name)


def test_tolerations_reach_the_pod_spec():
  c = _cluster()
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(1, 1)
  tol = [{"key": "workload", "operator": "Equal", "value": "sandbox", "effect": "NoSchedule"}]
  prepull(c, ["x"], tolerations=tol, wait=True, timeout=5)
  _, body = c.apps_api.create_namespaced_daemon_set.call_args.args
  assert body["spec"]["template"]["spec"]["tolerations"] == tol
  assert body["metadata"]["labels"]["role"] == "prepull"


def test_batches_are_paced_and_deleted_when_done():
  c = _Tracker()
  ok = prepull(c, [f"img{i}" for i in range(60)], images_per_pod=25, max_parallel=2,
               delete_when_done=True, wait=True, timeout=30, poll_interval=0)
  assert ok is True
  names = [b["metadata"]["name"] for b in c.created]
  assert names == ["agent-sandbox-rl-prepull-000", "agent-sandbox-rl-prepull-001",
                   "agent-sandbox-rl-prepull-002"]
  sizes = [len(b["spec"]["template"]["spec"]["initContainers"]) for b in c.created]
  assert sizes == [25, 25, 10]
  assert c.peak == 2 and c.deleted == names and not c.alive
  # each batch selects only its own pods
  sels = [b["spec"]["selector"]["matchLabels"]["prepull-ds"] for b in c.created]
  assert sels == names


def test_small_lists_keep_the_single_named_daemonset():
  c = _Tracker()
  assert prepull(c, ["a", "b"], images_per_pod=25, wait=True, timeout=5, poll_interval=0)
  assert [b["metadata"]["name"] for b in c.created] == ["agent-sandbox-rl-prepull"]
  assert c.deleted == []                     # kept by default: covers new nodes


def test_no_wait_creates_every_batch_without_pacing():
  c = _Tracker()
  assert prepull(c, [f"i{i}" for i in range(50)], images_per_pod=10, max_parallel=1, wait=False)
  assert len(c.created) == 5 and c.peak == 5
  c.apps_api.read_namespaced_daemon_set_status.assert_not_called()


def test_budget_exhausted_stops_starting_batches_and_returns_false():
  c = _Tracker(ready=False)
  ok = prepull(c, [f"i{i}" for i in range(30)], images_per_pod=10, max_parallel=1,
               wait=True, timeout=0, poll_interval=0)
  assert ok is False
  assert len(c.created) == 1                 # the rest never started


def test_zero_desired_grace_is_measured_per_batch(monkeypatch):
  clock = {"t": 0.0}

  def monotonic():
    clock["t"] += 10.0                        # every look at the clock is 10 s later
    return clock["t"]

  _no_sleep(monkeypatch, monotonic=monotonic)
  c = _Tracker()
  reads = {}

  def status(name, ns):                       # a fresh DaemonSet says 0/0 once
    reads[name] = reads.get(name, 0) + 1
    return _status(0, 0) if reads[name] == 1 else _status(2, 2)

  c.apps_api.read_namespaced_daemon_set_status.side_effect = status
  ok = prepull(c, [f"i{i}" for i in range(30)], images_per_pod=10, max_parallel=1,
               wait=True, timeout=1000, poll_interval=0)
  assert ok is True
  # each batch was polled past its own 0/0 reading, including the ones created
  # long after the 15 s grace from the start of the pre-pull had passed
  assert reads == {"agent-sandbox-rl-prepull-000": 2, "agent-sandbox-rl-prepull-001": 2,
                   "agent-sandbox-rl-prepull-002": 2}


def test_a_batch_that_missed_the_budget_is_kept_for_inspection():
  c = _Tracker(ready=False)
  ok = prepull(c, ["a"], delete_when_done=True, wait=True, timeout=0, poll_interval=0)
  assert ok is False and c.deleted == [] and c.alive == {"agent-sandbox-rl-prepull"}


def test_no_wait_warns_when_cleanup_or_drop_caches_cannot_run(caplog):
  c = _Tracker()
  with caplog.at_level("WARNING", logger="agent_sandbox_rl.prepull"):
    assert prepull(c, ["a"], drop_caches=True, wait=False) is True
  assert any("need wait=True" in r.message for r in caplog.records)
  assert len(c.created) == 1                  # no drop-caches DaemonSet


def test_drop_caches_runs_a_privileged_oneshot_after_the_pulls():
  c = _Tracker()
  tol = [{"key": "workload", "value": "sandbox"}]
  ok = prepull(c, ["a"], tolerations=tol, drop_caches=True, wait=True, timeout=10, poll_interval=0)
  assert ok is True
  names = [b["metadata"]["name"] for b in c.created]
  assert names == ["agent-sandbox-rl-prepull", "agent-sandbox-rl-prepull-drop-caches"]
  dc = c.created[1]["spec"]["template"]["spec"]
  init = dc["initContainers"][0]
  assert init["securityContext"] == {"privileged": True}
  assert "drop_caches" in init["command"][2] and dc["tolerations"] == tol
  assert c.deleted == ["agent-sandbox-rl-prepull-drop-caches"]   # the pull DS is kept
  # not run when a pull batch failed
  c2 = _Tracker(ready=False)
  assert prepull(c2, ["a"], drop_caches=True, wait=True, timeout=0, poll_interval=0) is False
  assert len(c2.created) == 1


def test_prepull_delete_removes_the_labeled_batches_too():
  c = _cluster()
  items = [types.SimpleNamespace(metadata=types.SimpleNamespace(name=n))
           for n in ("agent-sandbox-rl-prepull", "agent-sandbox-rl-prepull-003", "x-drop-caches")]
  c.apps_api.list_namespaced_daemon_set.return_value = types.SimpleNamespace(items=items)
  prepull_delete(c, labels={"app": "agent-sandbox-rl", "run": "r1"})
  sel = c.apps_api.list_namespaced_daemon_set.call_args.kwargs["label_selector"]
  assert sel == "app=agent-sandbox-rl,role=prepull"      # any run's, not only this run's
  deleted = [k.args[0] for k in c.apps_api.delete_namespaced_daemon_set.call_args_list]
  assert deleted == ["agent-sandbox-rl-prepull", "agent-sandbox-rl-prepull-003", "x-drop-caches"]


def test_object_labels_carry_the_cleanup_selector_even_when_the_caller_labels_replace_the_defaults():
  c = _cluster()
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(1, 1)
  prepull(c, ["x"], labels={"team": "x"}, wait=True, timeout=5)            # no app= label
  _, body = c.apps_api.create_namespaced_daemon_set.call_args.args
  meta = body["metadata"]["labels"]
  assert meta["team"] == "x" and meta["app"] == "agent-sandbox-rl" and meta["role"] == "prepull"
  assert meta["prepull-ds"] == "agent-sandbox-rl-prepull"


def test_recreation_of_a_terminating_daemonset_honors_the_budget(monkeypatch):
  _no_sleep(monkeypatch)
  c = _cluster()
  c.apps_api.create_namespaced_daemon_set.side_effect = client.ApiException(status=409)
  c.apps_api.read_namespaced_daemon_set.return_value = types.SimpleNamespace(
      spec=types.SimpleNamespace(selector=types.SimpleNamespace(
          match_labels={"app": "agent-sandbox-rl", "role": "prepull",
                        "agents.x-k8s.io/asrl-run-id": "old-run"})))
  assert prepull(c, ["x"], wait=True, timeout=0) is False                  # not an exception
  c.apps_api.delete_namespaced_daemon_set.assert_called_once()
  assert c.apps_api.create_namespaced_daemon_set.call_count == 1           # no retry past the budget


def test_drop_caches_rejected_by_pod_security_fails_at_once():
  c = _Tracker()
  real_create = c.apps_api.create_namespaced_daemon_set.side_effect

  def create(ns, body):
    if body["metadata"]["name"].endswith("-drop-caches"):
      raise client.ApiException(status=403, reason="violates PodSecurity \"restricted:latest\"")
    return real_create(ns, body)

  c.apps_api.create_namespaced_daemon_set.side_effect = create
  assert prepull(c, ["a"], drop_caches=True, wait=True, timeout=10, poll_interval=0) is False
  assert c.apps_api.read_namespaced_daemon_set_status.call_count == 1   # the pull batch only
