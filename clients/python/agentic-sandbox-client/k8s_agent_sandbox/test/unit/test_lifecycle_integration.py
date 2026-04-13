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

"""
Integration tests for the shutdown_after_seconds lifecycle feature.

These tests exercise the full code path from create_sandbox() through
to the K8s API call with real objects. Only the K8s API transport is
mocked. This validates that construct_sandbox_claim_lifecycle_spec(),
SandboxClient, and K8sHelper are wired correctly end-to-end.
"""

import unittest
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.sandbox_client import SandboxClient


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
@patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
class TestLifecycleIntegration(unittest.TestCase):
    """End-to-end integration: SandboxClient -> K8sHelper -> manifest body.

    Only the K8s API transport (CustomObjectsApi) is mocked.
    Everything else (construct_sandbox_claim_lifecycle_spec, _create_claim,
    manifest construction) runs the real code.
    """

    def test_create_sandbox_with_shutdown_produces_lifecycle_in_manifest(
        self, MockClientK8sHelper, mock_config, mock_api_cls, mock_core_cls
    ):
        """Full path: create_sandbox(shutdown_after_seconds=300) embeds
        spec.lifecycle in the K8s API call body."""
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        real_helper = K8sHelper.__new__(K8sHelper)
        real_helper.custom_objects_api = mock_api
        real_helper.core_v1_api = MagicMock()

        sandbox_client = SandboxClient.__new__(SandboxClient)
        sandbox_client.k8s_helper = real_helper
        sandbox_client.tracing_manager = None
        sandbox_client.tracer = MagicMock()
        sandbox_client.connection_config = MagicMock()
        sandbox_client.tracer_config = MagicMock()
        sandbox_client.tracer_config.enable_tracing = False
        sandbox_client._active_connection_sandboxes = {}
        sandbox_client.sandbox_class = MagicMock()

        real_helper.resolve_sandbox_name = MagicMock(return_value="sandbox-abc")
        real_helper.wait_for_sandbox_ready = MagicMock()

        before = datetime.now(timezone.utc)
        sandbox_client.create_sandbox("my-template", "default", shutdown_after_seconds=300)
        after = datetime.now(timezone.utc)

        mock_api.create_namespaced_custom_object.assert_called_once()
        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]

        self.assertIn("lifecycle", body["spec"])
        lifecycle = body["spec"]["lifecycle"]
        self.assertEqual(lifecycle["shutdownPolicy"], "Delete")

        shutdown_time = datetime.strptime(lifecycle["shutdownTime"], "%Y-%m-%dT%H:%M:%SZ")
        shutdown_time = shutdown_time.replace(tzinfo=timezone.utc)
        expected_earliest = before.replace(microsecond=0)
        expected_latest = after.replace(microsecond=0)
        self.assertGreaterEqual(shutdown_time, expected_earliest + timedelta(seconds=300))
        self.assertLessEqual(shutdown_time, expected_latest + timedelta(seconds=301))

        self.assertEqual(body["kind"], "SandboxClaim")
        self.assertEqual(body["spec"]["sandboxTemplateRef"]["name"], "my-template")

    def test_create_sandbox_without_shutdown_omits_lifecycle_in_manifest(
        self, MockClientK8sHelper, mock_config, mock_api_cls, mock_core_cls
    ):
        """Full path: create_sandbox() without shutdown_after_seconds
        produces no spec.lifecycle."""
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        real_helper = K8sHelper.__new__(K8sHelper)
        real_helper.custom_objects_api = mock_api
        real_helper.core_v1_api = MagicMock()

        sandbox_client = SandboxClient.__new__(SandboxClient)
        sandbox_client.k8s_helper = real_helper
        sandbox_client.tracing_manager = None
        sandbox_client.tracer = MagicMock()
        sandbox_client.connection_config = MagicMock()
        sandbox_client.tracer_config = MagicMock()
        sandbox_client.tracer_config.enable_tracing = False
        sandbox_client._active_connection_sandboxes = {}
        sandbox_client.sandbox_class = MagicMock()

        real_helper.resolve_sandbox_name = MagicMock(return_value="sandbox-abc")
        real_helper.wait_for_sandbox_ready = MagicMock()

        sandbox_client.create_sandbox("my-template", "default")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("lifecycle", body["spec"])
        self.assertEqual(body["spec"]["sandboxTemplateRef"]["name"], "my-template")

    def test_invalid_shutdown_never_reaches_k8s_api(
        self, MockClientK8sHelper, mock_config, mock_api_cls, mock_core_cls
    ):
        """Validation fires before any K8s API call."""
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        real_helper = K8sHelper.__new__(K8sHelper)
        real_helper.custom_objects_api = mock_api
        real_helper.core_v1_api = MagicMock()

        sandbox_client = SandboxClient.__new__(SandboxClient)
        sandbox_client.k8s_helper = real_helper
        sandbox_client.tracing_manager = None
        sandbox_client.tracer = MagicMock()
        sandbox_client.connection_config = MagicMock()
        sandbox_client.tracer_config = MagicMock()
        sandbox_client.tracer_config.enable_tracing = False
        sandbox_client._active_connection_sandboxes = {}
        sandbox_client.sandbox_class = MagicMock()

        with self.assertRaises(ValueError):
            sandbox_client.create_sandbox("my-template", shutdown_after_seconds=-1)

        mock_api.create_namespaced_custom_object.assert_not_called()


if __name__ == "__main__":
    unittest.main()
