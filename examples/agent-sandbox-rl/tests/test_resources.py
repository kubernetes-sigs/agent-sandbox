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

from unittest.mock import MagicMock

import pytest
from kubernetes import client

from agent_sandbox_rl import constants
from agent_sandbox_rl.config import TemplateSpec
from agent_sandbox_rl.resources import Resources

IMG = "slimshetty/swebench-verified:sweb.eval.x86_64.astropy__astropy-12907"
TNAME = "r2e-img-abc123"


def _resources():
  return Resources(MagicMock(), MagicMock(), "ns")


def test_ensure_template_creates_when_absent():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  created = r.ensure_template(IMG, TNAME, TemplateSpec(runtime_class="gvisor",
                              node_selector={"k": "v"}, image_pull_secret="ps"))
  assert created is True
  args, kwargs = r.custom_api.create_namespaced_custom_object.call_args
  body = kwargs["body"]
  assert kwargs["plural"] == constants.TEMPLATES_PLURAL
  assert body["kind"] == "SandboxTemplate"
  assert body["metadata"]["labels"] == constants.DEFAULT_LABELS
  pod = body["spec"]["podTemplate"]["spec"]
  assert pod["containers"][0]["image"] == IMG
  assert pod["containers"][0]["command"] == constants.KEEPALIVE_COMMAND
  assert pod["runtimeClassName"] == "gvisor"
  assert pod["nodeSelector"] == {"k": "v"}
  assert pod["imagePullSecrets"] == [{"name": "ps"}]


def test_ensure_template_noop_when_present():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"metadata": {"name": TNAME}}
  created = r.ensure_template(IMG, TNAME, TemplateSpec())
  assert created is False
  r.custom_api.create_namespaced_custom_object.assert_not_called()


def test_ensure_template_reraises_non_404():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=500)
  with pytest.raises(client.ApiException):
    r.ensure_template(IMG, TNAME, TemplateSpec())


def test_create_warmpool_body():
  r = _resources()
  r.create_warmpool("pool-x", TNAME, 3)
  _, kwargs = r.custom_api.create_namespaced_custom_object.call_args
  body = kwargs["body"]
  assert kwargs["plural"] == constants.WARMPOOLS_PLURAL
  assert body["kind"] == "SandboxWarmPool"
  assert body["spec"] == {"replicas": 3, "sandboxTemplateRef": {"name": TNAME}}


def test_ensure_template_dry_run_forwarded():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  r.ensure_template(IMG, TNAME, TemplateSpec(), dry_run=True)
  _, kwargs = r.custom_api.create_namespaced_custom_object.call_args
  assert kwargs["dry_run"] == "All"


def test_create_warmpool_dry_run_forwarded():
  r = _resources()
  r.create_warmpool("pool-x", TNAME, 1, dry_run=True)
  _, kwargs = r.custom_api.create_namespaced_custom_object.call_args
  assert kwargs["dry_run"] == "All"
  # default is a real create (no dry run)
  r2 = _resources()
  r2.create_warmpool("pool-y", TNAME, 1)
  _, kwargs2 = r2.custom_api.create_namespaced_custom_object.call_args
  assert kwargs2["dry_run"] is None


def test_validate_manifests_dry_runs_template_then_warmpool():
  r = _resources()
  r.validate_manifests(IMG, TemplateSpec())
  calls = r.custom_api.create_namespaced_custom_object.call_args_list
  assert len(calls) == 2
  assert calls[0].kwargs["plural"] == constants.TEMPLATES_PLURAL
  assert calls[0].kwargs["dry_run"] == "All"
  assert calls[1].kwargs["plural"] == constants.WARMPOOLS_PLURAL
  assert calls[1].kwargs["dry_run"] == "All"


def test_create_warmpool_swallows_409():
  r = _resources()
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  r.create_warmpool("pool-x", TNAME, 1)  # no raise


def test_delete_swallows_404():
  r = _resources()
  r.custom_api.delete_namespaced_custom_object.side_effect = client.ApiException(status=404)
  r.delete_warmpool("pool-x")   # no raise
  r.delete_template(TNAME)      # no raise


def test_pool_ready_replicas_reads_status():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 2}}
  assert r.pool_ready_replicas("pool-x") == 2


def test_wait_for_pool_ready_true_immediately():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 3}}
  assert r.wait_for_pool_ready("pool-x", 3, timeout=5) is True


def test_wait_for_pool_ready_times_out():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 0}}
  assert r.wait_for_pool_ready("pool-x", 1, timeout=0) is False


def test_wait_for_pool_ready_via_watch(monkeypatch):
  # Not ready on the fast-path GET; a watch MODIFIED event flips it to ready.
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 0}}

  class FakeWatch:
    def stream(self, func, **kw):
      return [
          {"type": "MODIFIED", "object": {"metadata": {"name": "other"},
                                          "status": {"readyReplicas": 9}}},   # ignored
          {"type": "MODIFIED", "object": {"metadata": {"name": "pool-x"},
                                          "status": {"readyReplicas": 2}}},
      ]

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: FakeWatch())
  assert r.wait_for_pool_ready("pool-x", 2, timeout=5) is True


def test_wait_for_pool_ready_scopes_watch_with_field_selector(monkeypatch):
  # The watch must be scoped server-side to this pool, not list all warmpools.
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 0}}
  seen = {}

  class _RecordingWatch:
    def stream(self, func, **kw):
      seen.update(kw)
      return [{"type": "MODIFIED", "object": {"metadata": {"name": "pool-x"},
                                              "status": {"readyReplicas": 1}}}]

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: _RecordingWatch())
  assert r.wait_for_pool_ready("pool-x", 1, timeout=5) is True
  assert seen.get("field_selector") == "metadata.name=pool-x"


def test_list_uses_label_selector():
  r = _resources()
  r.custom_api.list_namespaced_custom_object.return_value = {
      "items": [{"metadata": {"name": "a"}}, {"metadata": {"name": "b"}}]}
  assert r.list_warmpools(label_selector=r.managed_selector()) == ["a", "b"]
  _, kwargs = r.custom_api.list_namespaced_custom_object.call_args
  assert kwargs["label_selector"] == "app=agent-sandbox-rl"


def test_wait_for_pool_ready_raises_on_forbidden(monkeypatch):
  # A terminal API error (RBAC 403) should fail fast, not busy-loop to timeout.
  r = _resources()
  r.custom_api.get_namespaced_custom_object.return_value = {"status": {"readyReplicas": 0}}

  class _ForbiddenWatch:
    def stream(self, *a, **k):
      raise client.ApiException(status=403)

    def stop(self):
      pass

  monkeypatch.setattr("agent_sandbox_rl.resources.watch.Watch", lambda: _ForbiddenWatch())
  with pytest.raises(client.ApiException):
    r.wait_for_pool_ready("pool-x", 1, timeout=5)


def test_labels_always_include_management_label():
  # Custom labels must not drop app=agent-sandbox-rl (teardown selects on it).
  r = Resources(MagicMock(), MagicMock(), "ns", labels={"team": "rl"})
  assert r.labels["team"] == "rl"
  assert r.labels[constants.MANAGED_BY_LABEL] == constants.MANAGED_BY_VALUE
  # even if a custom label tries to override it
  r2 = Resources(MagicMock(), MagicMock(), "ns", labels={"app": "evil"})
  assert r2.labels["app"] == constants.MANAGED_BY_VALUE


def test_ensure_template_swallows_409():
  r = _resources()
  r.custom_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
  r.custom_api.create_namespaced_custom_object.side_effect = client.ApiException(status=409)
  assert r.ensure_template(IMG, TNAME, TemplateSpec()) is False   # no raise
