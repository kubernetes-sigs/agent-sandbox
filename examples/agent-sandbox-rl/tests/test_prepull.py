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

import types
from unittest.mock import MagicMock

from kubernetes import client

from agent_sandbox_rl import prepull, prepull_delete


def _cluster():
  return types.SimpleNamespace(name="c", namespace="ns", apps_api=MagicMock())


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


def test_existing_ds_is_patched():
  c = _cluster()
  c.apps_api.create_namespaced_daemon_set.side_effect = client.ApiException(status=409)
  c.apps_api.read_namespaced_daemon_set_status.return_value = _status(1, 1)
  assert prepull(c, ["x"], wait=True, timeout=5) is True
  c.apps_api.patch_namespaced_daemon_set.assert_called_once()


def test_delete_swallows_404():
  c = _cluster()
  c.apps_api.delete_namespaced_daemon_set.side_effect = client.ApiException(status=404)
  prepull_delete(c)  # no raise
