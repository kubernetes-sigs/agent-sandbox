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
    PODSNAPSHOT_NAMESPACE_MANAGED,
    PODSNAPSHOT_AGENT,
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
)

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

    def test_snapshot_controller_ready_success(self):
        """Test snapshot_controller_ready success scenarios (Direct & CRD Fallback)."""
        logging.info("TEST: Direct Managed Success")
        mock_v1 = self.client.core_v1_api
        mock_pod_agent = MagicMock()
        mock_pod_agent.metadata.name = PODSNAPSHOT_AGENT
        mock_pod_agent.status.phase = "Running"
        mock_pods = MagicMock()
        mock_pods.items = [mock_pod_agent]
        mock_v1.list_namespaced_pod.return_value = mock_pods

        self.client.controller_ready = False
        self.assertTrue(self.client.snapshot_controller_ready())
        mock_v1.list_namespaced_pod.assert_called_with(
            PODSNAPSHOT_NAMESPACE_MANAGED, label_selector=f"app={PODSNAPSHOT_AGENT}"
        )

        logging.info("TEST: CRD Fallback Success")
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=403)
        mock_resource_list = MagicMock()
        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list.resources = [mock_resource]
        self.client.custom_objects_api.get_api_resources.return_value = (
            mock_resource_list
        )

        self.client.controller_ready = False
        self.assertTrue(self.client.snapshot_controller_ready())
        self.client.custom_objects_api.get_api_resources.assert_called_with(
            group=PODSNAPSHOT_API_GROUP, version=PODSNAPSHOT_API_VERSION
        )

    def test_snapshot_controller_ready_failures(self):
        """Test snapshot_controller_ready failure scenarios."""
        mock_v1 = self.client.core_v1_api

        # 1. Pod missing (Not Ready)
        mock_v1.list_namespaced_pod.side_effect = None
        mock_pods = MagicMock()
        mock_pods.items = []
        mock_v1.list_namespaced_pod.return_value = mock_pods
        self.client.controller_ready = False
        self.assertFalse(self.client.snapshot_controller_ready())

        # 2. 404 on Pod List
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=404)
        self.client.controller_ready = False
        self.assertFalse(self.client.snapshot_controller_ready())

        # 3. Forbidden (403) + No CRDs found
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=403)
        self.client.custom_objects_api.get_api_resources.return_value = None
        self.client.controller_ready = False
        self.assertFalse(self.client.snapshot_controller_ready())

        # 4. Forbidden (403) + CRD Kind mismatch
        mock_resource_list = MagicMock()
        mock_resource = MagicMock()
        mock_resource.kind = "SomeOtherKind"
        mock_resource_list.resources = [mock_resource]
        self.client.custom_objects_api.get_api_resources.return_value = (
            mock_resource_list
        )
        self.client.controller_ready = False
        self.assertFalse(self.client.snapshot_controller_ready())

        # 5. Forbidden (403) + 404 on CRD check
        self.client.custom_objects_api.get_api_resources.side_effect = ApiException(
            status=404
        )
        self.client.controller_ready = False
        self.assertFalse(self.client.snapshot_controller_ready())

    def test_snapshot_controller_ready_exceptions(self):
        """Test API exceptions during snapshot readiness checks."""
        mock_v1 = self.client.core_v1_api

        # 1. 500 on Pod List
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=500)
        self.client.controller_ready = False
        with self.assertRaises(ApiException):
            self.client.snapshot_controller_ready()

        # 2. 403 on Pod List + 500 on CRD Check
        mock_v1.list_namespaced_pod.side_effect = ApiException(status=403)
        self.client.custom_objects_api.get_api_resources.side_effect = ApiException(
            status=500
        )
        self.client.controller_ready = False
        with self.assertRaises(ApiException):
            self.client.snapshot_controller_ready()

    def test_enter_exit(self):
        """Test context manager __enter__ implementation."""
        # Success path
        self.client.controller_ready = False
        with patch.object(
            self.client, "snapshot_controller_ready", return_value=True
        ) as mock_ready:
            with patch(
                "k8s_agent_sandbox.sandbox_client.SandboxClient.__enter__"
            ) as mock_super_enter:
                result = self.client.__enter__()
                self.assertEqual(result, self.client)
                mock_ready.assert_called_once()
                mock_super_enter.assert_called_once()
                self.assertTrue(self.client.controller_ready)

        # Failure path
        self.client.controller_ready = False
        with patch.object(
            self.client,
            "snapshot_controller_ready",
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

    def test_snapshot_controller_already_ready(self):
        """Test early return if snapshot controller is already ready."""
        self.client.controller_ready = True
        mock_v1 = self.client.core_v1_api
        result = self.client.snapshot_controller_ready()
        self.assertTrue(result)
        mock_v1.list_namespaced_pod.assert_not_called()


if __name__ == "__main__":
    unittest.main()
