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
from agentic_sandbox.gke_extensions.podsnapshot_client import (
    PodSnapshotSandboxClient,
)
from agentic_sandbox.constants import *
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
            "agentic_sandbox.sandbox_client.SandboxClient.__init__", return_value=None
        ) as mock_super:
            with patch.object(
                PodSnapshotSandboxClient, "snapshot_controller_ready", return_value=True
            ):
                client = PodSnapshotSandboxClient("test-template")
            mock_super.assert_called_once_with("test-template", server_port=8080)
        self.assertFalse(client.controller_ready)
        self.assertEqual(client.podsnapshot_timeout, 180)
        logging.info("Finished test_init.")

    @patch("agentic_sandbox.gke_extensions.podsnapshot_client.client.CoreV1Api")
    def test_snapshot_controller_ready_managed(self, mock_v1_class):
        """Test snapshot_controller_ready for managed scenario."""
        logging.info("Starting test_snapshot_controller_ready_managed...")
        mock_v1 = MagicMock()
        mock_v1_class.return_value = mock_v1

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

    @patch("agentic_sandbox.gke_extensions.podsnapshot_client.client.CoreV1Api")
    def test_snapshot_controller_ready_status_not_ready(self, mock_v1_class):
        """Test snapshot_controller_ready when not ready."""
        logging.info("Starting test_snapshot_controller_ready_status_not_ready...")
        mock_v1 = MagicMock()
        mock_v1_class.return_value = mock_v1

        mock_pods = MagicMock()
        mock_pods.items = []
        mock_v1.list_namespaced_pod.return_value = mock_pods

        self.client.controller_ready = False
        result = self.client.snapshot_controller_ready()

        self.assertFalse(result)
        self.assertFalse(self.client.controller_ready)
        logging.info("Finished test_snapshot_controller_ready_status_not_ready.")

    @patch("agentic_sandbox.gke_extensions.podsnapshot_client.watch.Watch")
    def test_snapshot_success(self, mock_watch_class):
        """Test successful snapshot creation."""
        logging.info("Starting test_snapshot_success...")

        # Mock the watch
        mock_watch = MagicMock()
        mock_watch_class.return_value = mock_watch

        self.client.pod_name = "test-pod"
        self.client.controller_ready = True
        self.client.namespace = "test-ns"

        # Mock the watch stream
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "status": {
                    "conditions": [
                        {
                            "type": "Triggered",
                            "status": "True",
                            "reason": "Complete",
                            "lastTransitionTime": "2023-01-01T00:00:00Z",
                        }
                    ],
                    "snapshotCreated": {"name": "snapshot-uid"},
                }
            },
        }
        mock_watch.stream.return_value = [mock_event]

        result = self.client.snapshot("test-trigger")

        self.assertEqual(result.error_code, 0)
        self.assertTrue(result.success)
        self.assertIn("test-trigger", result.trigger_name)

        # Verify create call was made
        self.client.custom_objects_api.create_namespaced_custom_object.assert_called_once()
        logging.info("Finished test_snapshot_success.")

    def test_snapshot_controller_not_ready(self):
        """Test snapshot when controller is not ready."""
        logging.info("Starting test_snapshot_controller_not_ready...")
        self.client.controller_ready = False
        result = self.client.snapshot("test-trigger")

        self.assertEqual(result.error_code, 1)
        self.assertFalse(result.success)
        self.assertIn("test-trigger", result.trigger_name)
        self.assertIn("Snapshot controller is not ready", result.error_reason)
        logging.info("Finished test_snapshot_controller_not_ready.")

    def test_snapshot_no_pod_name(self):
        """Test snapshot when pod name is not set."""
        logging.info("Starting test_snapshot_no_pod_name...")
        self.client.controller_ready = True
        self.client.pod_name = None
        result = self.client.snapshot("test-trigger")

        self.assertEqual(result.error_code, 1)
        self.assertFalse(result.success)
        self.assertIn("test-trigger", result.trigger_name)
        self.assertIn("Sandbox pod name not found", result.error_reason)
        logging.info("Finished test_snapshot_no_pod_name.")

    @patch("agentic_sandbox.gke_extensions.podsnapshot_client.watch.Watch")
    @patch("agentic_sandbox.gke_extensions.podsnapshot_client.client.CustomObjectsApi")
    def test_snapshot_timeout(self, mock_custom_class, mock_watch_class):
        """Test snapshot timeout scenario."""
        logging.info("Starting test_snapshot_timeout...")
        mock_custom = MagicMock()
        mock_custom_class.return_value = mock_custom

        mock_watch = MagicMock()
        mock_watch_class.return_value = mock_watch

        self.client.pod_name = "test-pod"
        self.client.controller_ready = True
        self.client.podsnapshot_timeout = 1

        # Mock empty stream (timeout)
        mock_watch.stream.return_value = []

        result = self.client.snapshot("test-trigger")

        self.assertEqual(result.error_code, 1)
        self.assertFalse(result.success)
        self.assertIn("timed out", result.error_reason)
        logging.info("Finished test_snapshot_timeout.")

    @patch("agentic_sandbox.gke_extensions.podsnapshot_client.SandboxClient.__exit__")
    def test_exit(self, mock_super_exit):
        """Test __exit__ method."""
        logging.info("Starting test_exit...")
        self.client.__exit__(None, None, None)
        mock_super_exit.assert_called_once_with(None, None, None)
        logging.info("Finished test_exit.")


if __name__ == "__main__":
    unittest.main()
