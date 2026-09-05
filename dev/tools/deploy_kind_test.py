# Copyright 2025 The Kubernetes Authors.
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

"""Unit tests for deploy-kind — orchestration and env-based configuration."""

import importlib.util
import os
import sys
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

# Load the extensionless deploy-kind script via importlib (same pattern as
# dev/tools/push_images_test.py).
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)
_REPO_ROOT = os.path.dirname(os.path.dirname(_SCRIPT_DIR))

_SCRIPT_PATH = os.path.join(_SCRIPT_DIR, "deploy-kind")
_loader = SourceFileLoader("deploy_kind", _SCRIPT_PATH)
_spec = importlib.util.spec_from_loader("deploy_kind", _loader)
deploy_kind = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(deploy_kind)


class DeployKindTest(unittest.TestCase):
    def _run_with(self, **env):
        with mock.patch.dict(os.environ, {"PATH": os.environ.get("PATH", ""), **env},
                             clear=True), \
             mock.patch.object(deploy_kind, "_run_tool") as run_tool:
            deploy_kind.main()
        return [call.args for call in run_tool.call_args_list]

    def _find_call(self, calls, tool):
        match = next((args for args in calls if args[0] == tool), None)
        return None if match is None else list(match[1:])

    def _assert_create_call(self, calls, cluster, engine):
        self.assertEqual(
            self._find_call(calls, "create-kind-cluster"),
            ["--recreate", cluster, "--kubeconfig",
             os.path.join(_REPO_ROOT, "bin", "KUBECONFIG"),
             "--container-engine", engine])

    def test_env_flag_accepts_true_and_one_case_insensitive(self):
        for value in ("true", "TRUE", "True", "1", " 1 "):
            with mock.patch.dict(os.environ, {"FLAG": value}, clear=True):
                self.assertTrue(deploy_kind._env_flag("FLAG"))

    def test_env_flag_rejects_other_values(self):
        for value in ("false", "FALSE", "0", "yes", "", " false "):
            with mock.patch.dict(os.environ, {"FLAG": value}, clear=True):
                self.assertFalse(deploy_kind._env_flag("FLAG"))

    def test_default_flow(self):
        calls = self._run_with()
        self._assert_create_call(calls, "agent-sandbox", "docker")
        self.assertEqual(self._find_call(calls, "push-images"),
                         ["--image-prefix=kind.local/",
                          "--kind-cluster-name=agent-sandbox",
                          "--container-engine=docker"])
        self.assertEqual(self._find_call(calls, "deploy-to-kube"),
                         ["--image-prefix=kind.local/", "--image-tag="])

    def test_skip_build_deploys_published_images(self):
        with mock.patch.object(deploy_kind, "_resolve_published_tag",
                               return_value="v0.5.5"):
            calls = self._run_with(SKIP_BUILD="true")
        self.assertIsNone(self._find_call(calls, "push-images"))
        self.assertEqual(self._find_call(calls, "deploy-to-kube"),
                         ["--image-prefix=registry.k8s.io/agent-sandbox/",
                          "--image-tag=v0.5.5"])

    def test_skip_build_with_pinned_image_tag(self):
        with mock.patch.object(deploy_kind, "_resolve_published_tag") as resolve:
            calls = self._run_with(SKIP_BUILD="true", IMAGE_TAG="v0.5.4")
        resolve.assert_not_called()
        self.assertEqual(self._find_call(calls, "deploy-to-kube"),
                         ["--image-prefix=registry.k8s.io/agent-sandbox/",
                          "--image-tag=v0.5.4"])

    def test_extensions_and_controller_flags(self):
        calls = self._run_with(EXTENSIONS="true",
                               CONTROLLER_ARGS="--zap-log-level=debug")
        self.assertEqual(self._find_call(calls, "deploy-to-kube"),
                         ["--image-prefix=kind.local/", "--image-tag=",
                          "--extensions", "--controller-args=--zap-log-level=debug"])

    def test_controller_only_builds_just_the_controller(self):
        calls = self._run_with(CONTROLLER_ONLY="true")
        self.assertEqual(self._find_call(calls, "push-images")[-1],
                         "--controller-only")

    def test_custom_engine_and_cluster(self):
        calls = self._run_with(CONTAINER_ENGINE="podman", KIND_CLUSTER_NAME="other")
        self._assert_create_call(calls, "other", "podman")
        self.assertIn("--container-engine=podman",
                      self._find_call(calls, "push-images"))


if __name__ == "__main__":
    unittest.main()
