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
import logging
from unittest.mock import MagicMock, patch
from kubernetes.client import ApiException
from k8s_agent_sandbox.gke_extensions.snapshots.podsnapshot_client import (
    PodSnapshotSandboxClient,
)
from k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support import (
    SandboxWithSnapshotSupport,
)
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
)

logger = logging.getLogger(__name__)


class TestPodSnapshotSandboxClient(unittest.TestCase):
    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_init_crd_installed_success(self, mock_k8s_helper_cls):
        mock_k8s_helper = mock_k8s_helper_cls.return_value
        mock_resource_list = MagicMock()
        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list.resources = [mock_resource]
        mock_k8s_helper.custom_objects_api.get_api_resources.return_value = (
            mock_resource_list
        )

        client = PodSnapshotSandboxClient()

        self.assertTrue(client.snapshot_crd_installed)
        mock_k8s_helper.custom_objects_api.get_api_resources.assert_called_with(
            group=PODSNAPSHOT_API_GROUP, version=PODSNAPSHOT_API_VERSION
        )

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_init_crd_not_installed_failure(self, mock_k8s_helper_cls):
        mock_k8s_helper = mock_k8s_helper_cls.return_value
        mock_k8s_helper.custom_objects_api.get_api_resources.return_value = None

        with self.assertRaises(RuntimeError) as context:
            PodSnapshotSandboxClient()

        self.assertIn("Pod Snapshot Controller is not ready", str(context.exception))

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_init_crd_api_exception(self, mock_k8s_helper_cls):
        mock_k8s_helper = mock_k8s_helper_cls.return_value
        mock_k8s_helper.custom_objects_api.get_api_resources.side_effect = ApiException(
            status=500
        )

        with self.assertRaises(ApiException):
            PodSnapshotSandboxClient()

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    @patch.object(
        PodSnapshotSandboxClient, "_check_snapshot_crd_installed", return_value=True
    )
    def test_sandbox_class(self, mock_check, mock_k8s_helper_cls):
        client = PodSnapshotSandboxClient()
        self.assertEqual(client.sandbox_class, SandboxWithSnapshotSupport)

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    @patch.object(
        PodSnapshotSandboxClient, "_check_snapshot_crd_installed", return_value=True
    )
    @patch("k8s_agent_sandbox.sandbox_client.SandboxClient.create_sandbox")
    def test_create_sandbox(self, mock_super_create, mock_check, mock_k8s_helper_cls):
        client = PodSnapshotSandboxClient()
        mock_super_create.return_value = MagicMock(spec=SandboxWithSnapshotSupport)

        result = client.create_sandbox("test-template", "test-ns")

        mock_super_create.assert_called_once_with("test-template", "test-ns")
        self.assertIsInstance(result, SandboxWithSnapshotSupport)

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    @patch.object(
        PodSnapshotSandboxClient, "_check_snapshot_crd_installed", return_value=True
    )
    @patch("k8s_agent_sandbox.sandbox_client.SandboxClient.get_sandbox")
    def test_get_sandbox(self, mock_super_get, mock_check, mock_k8s_helper_cls):
        client = PodSnapshotSandboxClient()
        mock_super_get.return_value = MagicMock(spec=SandboxWithSnapshotSupport)

        result = client.get_sandbox("test-id", "test-ns")

        mock_super_get.assert_called_once_with("test-id", "test-ns")
        self.assertIsInstance(result, SandboxWithSnapshotSupport)

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    @patch.object(
        PodSnapshotSandboxClient, "_check_snapshot_crd_installed", return_value=True
    )
    @patch("k8s_agent_sandbox.sandbox_client.SandboxClient.list_active_sandboxes")
    def test_list_active_sandboxes(
        self, mock_super_list, mock_check, mock_k8s_helper_cls
    ):
        client = PodSnapshotSandboxClient()
        mock_super_list.return_value = ["test-id-1", "test-id-2"]

        result = client.list_active_sandboxes()

        mock_super_list.assert_called_once_with()
        self.assertEqual(result, ["test-id-1", "test-id-2"])


if __name__ == "__main__":
    unittest.main()
