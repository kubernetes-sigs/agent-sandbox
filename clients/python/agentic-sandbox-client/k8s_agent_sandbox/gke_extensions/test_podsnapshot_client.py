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
from unittest.mock import MagicMock, patch, call
from datetime import datetime
from k8s_agent_sandbox.gke_extensions.podsnapshot_client import (
    PodSnapshotSandboxClient,
)
from k8s_agent_sandbox.constants import *
from kubernetes.client import ApiException

from kubernetes import config

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s"
)


def load_kubernetes_config():
    """Loads Kubernetes configuration, prioritizing kubeconfig and falling back to an environment variable."""
    try:
        config.load_kube_config()
        logging.info("Kubernetes config loaded from kubeconfig file.")
    except config.ConfigException:
        logging.info(
            "Kubeconfig file not found, attempting to load from environment variable."
        )
        try:
            config.load_kube_config(config_file=os.getenv("KUBECONFIG_FILE"))
            logging.info(
                "Kubernetes config loaded from KUBECONFIG_FILE environment variable."
            )
        except Exception as e:
            logging.error(f"Could not load Kubernetes config: {e}", exc_info=True)
            raise


class TestPodSnapshotSandboxClient(unittest.TestCase):

    @patch("kubernetes.config")
    def setUp(self, mock_config):
        logging.info("Setting up TestPodSnapshotSandboxClient...")
        # Mock kubernetes config loading
        mock_config.load_incluster_config.side_effect = config.ConfigException(
            "Not in cluster"
        )
        mock_config.load_kube_config.return_value = None

        # Create client without patching super, as it's tested separately
        with patch.object(
            PodSnapshotSandboxClient, "snapshot_controller_ready", return_value=True
        ):
            self.client = PodSnapshotSandboxClient("test-template")

        # Mock the kubernetes APIs on the client instance
        self.client.custom_objects_api = MagicMock()
        self.client.core_v1_api = MagicMock()

        logging.info("Finished setting up TestPodSnapshotSandboxClient.")

    def test_init(self):
        """Test initialization of PodSnapshotSandboxClient."""
        logging.info("Starting test_init...")
        with patch(
            "k8s_agent_sandbox.sandbox_client.SandboxClient.__init__", return_value=None
        ) as mock_super:
            with patch.object(
                PodSnapshotSandboxClient, "snapshot_controller_ready", return_value=True
            ):
                client = PodSnapshotSandboxClient("test-template")
            mock_super.assert_called_once_with("test-template", server_port=8080)
        self.assertFalse(client.controller_ready)
        self.assertEqual(client.podsnapshot_timeout, 180)
        logging.info("Finished test_init.")

    def test_snapshot_controller_ready_managed(self):
        """Test snapshot_controller_ready for managed scenario."""
        logging.info("Starting test_snapshot_controller_ready_managed...")
        mock_v1 = self.client.core_v1_api

        # Mock pods in gke-managed-pod-snapshots
        mock_pod_agent = MagicMock()
        mock_pod_agent.metadata.name = "pod-snapshot-agent"
        mock_pod_agent.status.phase = "Running"

        mock_pods = MagicMock()
        mock_pods.items = [mock_pod_agent]
        mock_v1.list_namespaced_pod.return_value = mock_pods

        self.client.controller_ready = False
        result = self.client.snapshot_controller_ready()

        self.assertTrue(result)
        self.assertTrue(self.client.controller_ready)
        mock_v1.list_namespaced_pod.assert_called_with(SNAPSHOT_NAMESPACE_MANAGED)
        logging.info("Finished test_snapshot_controller_ready_managed.")

    def test_snapshot_controller_ready_status_not_ready(self):
        """Test snapshot_controller_ready when not ready (pod missing)."""
        logging.info("Starting test_snapshot_controller_ready_status_not_ready...")
        mock_v1 = self.client.core_v1_api

        mock_pods = MagicMock()
        mock_pods.items = []
        mock_v1.list_namespaced_pod.return_value = mock_pods

        self.client.controller_ready = False
        result = self.client.snapshot_controller_ready()

        self.assertFalse(result)
        self.assertFalse(self.client.controller_ready)
        logging.info("Finished test_snapshot_controller_ready_status_not_ready.")

    def test_snapshot_controller_ready_forbidden_with_crd(self):
        """Test fallback to CRD check when pod listing is forbidden."""
        logging.info("Starting test_snapshot_controller_ready_forbidden_with_crd...")
        mock_v1 = self.client.core_v1_api
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=403)

        # Mock CustomObjectsApi.get_api_resources
        mock_resource_list = MagicMock()
        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list.resources = [mock_resource]
        self.client.custom_objects_api.get_api_resources.return_value = (
            mock_resource_list
        )

        self.client.controller_ready = False
        result = self.client.snapshot_controller_ready()

        self.assertTrue(result)
        self.assertTrue(self.client.controller_ready)
        self.client.custom_objects_api.get_api_resources.assert_called_with(
            group=PODSNAPSHOT_API_GROUP, version=PODSNAPSHOT_API_VERSION
        )
        logging.info("Finished test_snapshot_controller_ready_forbidden_with_crd.")

    def test_snapshot_controller_ready_forbidden_no_crd(self):
        """Test fallback to CRD check fails when CRD is missing."""
        logging.info("Starting test_snapshot_controller_ready_forbidden_no_crd...")
        mock_v1 = self.client.core_v1_api
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=403)

        # Mock CustomObjectsApi.get_api_resources returning empty
        self.client.custom_objects_api.get_api_resources.return_value = None

        self.client.controller_ready = False
        result = self.client.snapshot_controller_ready()

        self.assertFalse(result)
        self.assertFalse(self.client.controller_ready)
        logging.info("Finished test_snapshot_controller_ready_forbidden_no_crd.")

    def test_snapshot_controller_ready_404(self):
        """Test snapshot_controller_ready returns False on 404."""
        logging.info("Starting test_snapshot_controller_ready_404...")
        mock_v1 = self.client.core_v1_api
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=404)

        self.client.controller_ready = False
        result = self.client.snapshot_controller_ready()

        self.assertFalse(result)
        self.assertFalse(self.client.controller_ready)
        logging.info("Finished test_snapshot_controller_ready_404.")


if __name__ == "__main__":
    unittest.main()
