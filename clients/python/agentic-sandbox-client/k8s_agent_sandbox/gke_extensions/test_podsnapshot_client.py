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

    @patch("k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch")
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

        # Mock create to return an object with resourceVersion
        mock_created_obj = {"metadata": {"resourceVersion": "123"}, "status": {}}
        self.client.custom_objects_api.create_namespaced_custom_object.return_value = (
            mock_created_obj
        )

        result = self.client.snapshot("test-trigger")

        self.assertEqual(result.error_code, 0)
        self.assertTrue(result.success)
        self.assertIn("test-trigger", result.trigger_name)

        # Verify create call was made
        self.client.custom_objects_api.create_namespaced_custom_object.assert_called_once()
        # Verify watch was called with resource_version
        mock_watch.stream.assert_called_once()
        _, kwargs = mock_watch.stream.call_args
        self.assertEqual(kwargs.get("resource_version"), "123")
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

    def test_snapshot_creation_api_exception(self):
        """Test snapshot handling of API exception during creation."""
        logging.info("Starting test_snapshot_creation_api_exception...")
        self.client.pod_name = "test-pod"
        self.client.controller_ready = True

        self.client.custom_objects_api.create_namespaced_custom_object.side_effect = (
            ApiException("Create failed")
        )

        result = self.client.snapshot("test-trigger")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Failed to create PodSnapshotManualTrigger", result.error_reason)
        logging.info("Finished test_snapshot_creation_api_exception.")

    @patch("k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch")
    @patch(
        "k8s_agent_sandbox.gke_extensions.podsnapshot_client.client.CustomObjectsApi"
    )
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

    @patch("k8s_agent_sandbox.gke_extensions.podsnapshot_client.SandboxClient.__exit__")
    def test_exit_cleanup(self, mock_super_exit):
        """Test __exit__ cleans up created triggers."""
        logging.info("Starting test_exit_cleanup...")
        self.client.created_manual_triggers = ["trigger-1", "trigger-2"]

        self.client.__exit__(None, None, None)

        # Check deletion calls
        self.assertEqual(
            self.client.custom_objects_api.delete_namespaced_custom_object.call_count, 2
        )

        calls = [
            call(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.client.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                name="trigger-1",
            ),
            call(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.client.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                name="trigger-2",
            ),
        ]
        self.client.custom_objects_api.delete_namespaced_custom_object.assert_has_calls(
            calls, any_order=True
        )

        mock_super_exit.assert_called_once_with(None, None, None)
        logging.info("Finished test_exit_cleanup.")

    def test_is_restored_from_snapshot_success(self):
        """Test is_restored_from_snapshot success case."""
        logging.info("Starting test_is_restored_from_snapshot_success...")
        self.client.pod_name = "test-pod"

        mock_pod = MagicMock()
        mock_condition = MagicMock()
        mock_condition.type = "PodRestored"
        mock_condition.status = "True"
        mock_condition.message = "Restored from snapshot-uid-123"
        mock_pod.status.conditions = [mock_condition]

        self.client.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.client.is_restored_from_snapshot("snapshot-uid-123")

        self.assertTrue(result.success)
        self.assertEqual(result.error_code, 0)
        logging.info("Finished test_is_restored_from_snapshot_success.")

    def test_is_restored_from_snapshot_mismatch(self):
        """Test is_restored_from_snapshot when UID matches another snapshot."""
        logging.info("Starting test_is_restored_from_snapshot_mismatch...")
        self.client.pod_name = "test-pod"

        mock_pod = MagicMock()
        mock_condition = MagicMock()
        mock_condition.type = "PodRestored"
        mock_condition.status = "True"
        mock_condition.message = "Restored from snapshot-uid-456"
        mock_pod.status.conditions = [mock_condition]

        self.client.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.client.is_restored_from_snapshot("snapshot-uid-123")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("not restored from the given snapshot", result.error_reason)
        logging.info("Finished test_is_restored_from_snapshot_mismatch.")

    def test_is_restored_from_snapshot_no_condition(self):
        """Test is_restored_from_snapshot when PodRestored condition is missing."""
        logging.info("Starting test_is_restored_from_snapshot_no_condition...")
        self.client.pod_name = "test-pod"

        mock_pod = MagicMock()
        mock_pod.status.conditions = []

        self.client.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.client.is_restored_from_snapshot("snapshot-uid-123")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Pod status or conditions not found", result.error_reason)
        logging.info("Finished test_is_restored_from_snapshot_no_condition.")

    def test_is_restored_from_snapshot_api_error(self):
        """Test is_restored_from_snapshot API exception handling."""
        logging.info("Starting test_is_restored_from_snapshot_api_error...")
        self.client.pod_name = "test-pod"
        self.client.core_v1_api.read_namespaced_pod.side_effect = ApiException(
            "API Error"
        )

        result = self.client.is_restored_from_snapshot("snapshot-uid-123")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Failed to check pod restore status", result.error_reason)
        logging.info("Finished test_is_restored_from_snapshot_api_error.")

    def test_is_restored_from_snapshot_no_pod_name(self):
        """Test is_restored_from_snapshot when pod_name is missing."""
        logging.info("Starting test_is_restored_from_snapshot_no_pod_name...")
        self.client.pod_name = None
        result = self.client.is_restored_from_snapshot("snapshot-uid-123")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Pod name not found", result.error_reason)
        logging.info("Finished test_is_restored_from_snapshot_no_pod_name.")

    def test_is_restored_from_snapshot_empty_uid(self):
        """Test is_restored_from_snapshot with empty UID."""
        logging.info("Starting test_is_restored_from_snapshot_empty_uid...")
        result = self.client.is_restored_from_snapshot("")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Snapshot UID cannot be empty", result.error_reason)
        logging.info("Finished test_is_restored_from_snapshot_empty_uid.")


if __name__ == "__main__":
    unittest.main()
