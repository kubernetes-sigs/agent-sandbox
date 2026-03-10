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

import unittest
import os
import logging
from unittest.mock import MagicMock, patch
from k8s_agent_sandbox.gke_extensions.podsnapshot_client import (
    PodSnapshotSandboxClient,
)
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
)

from kubernetes.client import ApiException

from kubernetes import config

logger = logging.getLogger(__name__)


class TestPodSnapshotSandboxClient(unittest.TestCase):

    @patch("kubernetes.config")
    def setUp(self, mock_config):
        logger.info("Setting up TestPodSnapshotSandboxClient...")
        # Mock kubernetes config loading
        mock_config.load_incluster_config.side_effect = config.ConfigException(
            "Not in cluster"
        )
        mock_config.load_kube_config.return_value = None

        # Create client without patching super, as it's tested separately
        with patch.object(
            PodSnapshotSandboxClient, "_check_snapshot_crd_installed", return_value=True
        ):
            self.client = PodSnapshotSandboxClient("test-template")

        # Mock the kubernetes APIs on the client instance
        self.client.custom_objects_api = MagicMock()
        self.client.core_v1_api = MagicMock()

        logger.info("Finished setting up TestPodSnapshotSandboxClient.")

    def test_init(self):
        """Test initialization of PodSnapshotSandboxClient."""
        logger.info("Starting test_init...")
        with patch(
            "k8s_agent_sandbox.sandbox_client.SandboxClient.__init__", return_value=None
        ) as mock_super:
            with patch.object(
                PodSnapshotSandboxClient,
                "_check_snapshot_crd_installed",
                return_value=True,
            ):
                client = PodSnapshotSandboxClient("test-template")
            mock_super.assert_called_once_with("test-template")
        self.assertFalse(client.snapshot_crd_installed)
        logger.info("Finished test_init.")

    def test_check_snapshot_crd_installed_success(self):
        """Test _check_snapshot_crd_installed success scenarios (Check CRD Existence)."""
        logger.info("TEST: CRD Existence Success")
        mock_resource_list = MagicMock()
        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list.resources = [mock_resource]
        self.client.custom_objects_api.get_api_resources.return_value = (
            mock_resource_list
        )

        self.client.snapshot_crd_installed = False
        self.assertTrue(self.client._check_snapshot_crd_installed())
        self.client.custom_objects_api.get_api_resources.assert_called_with(
            group=PODSNAPSHOT_API_GROUP, version=PODSNAPSHOT_API_VERSION
        )

    def test_check_snapshot_crd_installed_failures(self):
        """Test _check_snapshot_crd_installed failure scenarios."""

        # 1. No CRDs found
        self.client.custom_objects_api.get_api_resources.return_value = None
        self.client.snapshot_crd_installed = False
        self.assertFalse(self.client._check_snapshot_crd_installed())

        # 2. CRD Kind mismatch
        mock_resource_list = MagicMock()
        mock_resource = MagicMock()
        mock_resource.kind = "SomeOtherKind"
        mock_resource_list.resources = [mock_resource]
        self.client.custom_objects_api.get_api_resources.return_value = (
            mock_resource_list
        )
        self.client.snapshot_crd_installed = False
        self.assertFalse(self.client._check_snapshot_crd_installed())

        # 3. 404 on CRD check
        self.client.custom_objects_api.get_api_resources.side_effect = ApiException(
            status=404
        )
        self.client.snapshot_crd_installed = False
        self.assertFalse(self.client._check_snapshot_crd_installed())

        # 4. 403 on CRD check
        self.client.custom_objects_api.get_api_resources.side_effect = ApiException(
            status=403
        )
        self.client.snapshot_crd_installed = False
        self.assertFalse(self.client._check_snapshot_crd_installed())

    def test_check_snapshot_crd_installed_exceptions(self):
        """Test API exceptions during snapshot readiness checks."""

        # 1. 500 on CRD Check
        self.client.custom_objects_api.get_api_resources.side_effect = ApiException(
            status=500
        )
        self.client.snapshot_crd_installed = False
        with self.assertRaises(ApiException):
            self.client._check_snapshot_crd_installed()

    def test_enter_exit(self):
        """Test context manager __enter__ implementation."""
        # Success path
        self.client.snapshot_crd_installed = False
        with patch.object(
            self.client, "_check_snapshot_crd_installed", return_value=True
        ) as mock_ready:
            with patch(
                "k8s_agent_sandbox.sandbox_client.SandboxClient.__enter__"
            ) as mock_super_enter:
                result = self.client.__enter__()
                self.assertEqual(result, self.client)
                mock_ready.assert_called_once()
                mock_super_enter.assert_called_once()
                self.assertTrue(self.client.snapshot_crd_installed)

        # Failure path: Controller not ready (return False)
        self.client.snapshot_crd_installed = False
        with patch.object(
            self.client, "_check_snapshot_crd_installed", return_value=False
        ) as mock_ready:
            with patch.object(self.client, "__exit__") as mock_exit:
                with self.assertRaises(RuntimeError) as context:
                    self.client.__enter__()
                self.assertIn(
                    "Pod Snapshot Controller is not ready",
                    str(context.exception),
                )
                mock_exit.assert_called_once_with(None, None, None)

        # Failure path: Exception during check
        self.client.snapshot_crd_installed = False
        with patch.object(
            self.client,
            "_check_snapshot_crd_installed",
            side_effect=ValueError("Test error"),
        ) as mock_ready:
            with patch.object(self.client, "__exit__") as mock_exit:
                with self.assertRaises(RuntimeError) as context:
                    self.client.__enter__()
                self.assertIn(
                    "Failed to initialize PodSnapshotSandboxClient",
                    str(context.exception),
                )
                mock_exit.assert_called_once_with(None, None, None)

        # Test Exit
        with patch(
            "k8s_agent_sandbox.sandbox_client.SandboxClient.__exit__"
        ) as mock_super_exit:
            exc_val = ValueError("test")
            self.client.__exit__(ValueError, exc_val, None)
            mock_super_exit.assert_called_once_with(ValueError, exc_val, None)

    def test_check_snapshot_crd_installed_already_ready(self):
        """Test early return if snapshot controller is already ready."""
        self.client.snapshot_crd_installed = True
        result = self.client._check_snapshot_crd_installed()
        self.assertTrue(result)
        self.client.custom_objects_api.get_api_resources.assert_not_called()


if __name__ == "__main__":
    unittest.main()
