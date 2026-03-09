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
from unittest.mock import MagicMock, patch, call
from k8s_agent_sandbox.gke_extensions.podsnapshot_client import (
    PodSnapshotSandboxClient,
)
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
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

    @patch("k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch")
    def test_snapshot_success(self, mock_watch_cls):
        """Test successful snapshot creation."""
        logging.info("Starting test_snapshot_success...")

        # Mock the watch
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True
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

    @patch("k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch")
    def test_snapshot_processed_retry(self, mock_watch_cls):
        """Test that snapshot waits for 'Complete' status, ignoring intermediate states."""
        logging.info("Starting test_snapshot_processed_retry...")

        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True
        self.client.namespace = "test-ns"

        # Mock events:
        # 1. Triggered but not complete (should raise ValueError internally and retry)
        # 2. Triggered and Complete (should succeed)
        event_incomplete = {
            "type": "MODIFIED",
            "object": {
                "status": {
                    "conditions": [
                        {
                            "type": "Triggered",
                            "status": "False",  # Not complete yet
                            "reason": "Pending",
                        }
                    ]
                }
            },
        }
        event_complete = {
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
                    "snapshotCreated": {"name": "snapshot-uid-retry"},
                }
            },
        }

        mock_watch.stream.return_value = [event_incomplete, event_complete]

        # Mock create object
        self.client.custom_objects_api.create_namespaced_custom_object.return_value = {
            "metadata": {"resourceVersion": "999"}
        }

        result = self.client.snapshot("test-retry")

        self.assertTrue(result.success)
        self.assertEqual(result.snapshot_uid, "snapshot-uid-retry")
        logging.info("Finished test_snapshot_processed_retry.")

    def test_snapshot_no_pod_name(self):
        """Test snapshot when pod name is not set."""
        logging.info("Starting test_snapshot_no_pod_name...")
        self.client.snapshot_crd_installed = True
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
        self.client.snapshot_crd_installed = True

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
    def test_snapshot_timeout(self, mock_custom_cls, mock_watch_cls):
        """Test snapshot timeout scenario."""
        logging.info("Starting test_snapshot_timeout...")
        mock_custom = MagicMock()
        mock_custom_cls.return_value = mock_custom

        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True
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

    def test_snapshot_watch_failure_condition(self):
        """Test snapshot failure when watch event reports 'False' status."""
        logging.info("Starting test_snapshot_watch_failure_condition...")
        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True

        # Mock watch to return failure event
        mock_watch = MagicMock()
        with patch(
            "k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch"
        ) as mock_watch_cls:
            mock_watch_cls.return_value = mock_watch
            failure_event = {
                "type": "MODIFIED",
                "object": {
                    "status": {
                        "conditions": [
                            {
                                "type": "Triggered",
                                "status": "False",
                                "reason": "Failed",
                                "message": "Snapshot failed due to timeout",
                            }
                        ]
                    }
                },
            }
            mock_watch.stream.return_value = [failure_event]

            # Mock create to return resource version
            self.client.custom_objects_api.create_namespaced_custom_object.return_value = {
                "metadata": {"resourceVersion": "100"}
            }

            result = self.client.snapshot("test-trigger-fail")

            self.assertFalse(result.success)
            self.assertEqual(result.error_code, 1)
            self.assertIn(
                "Snapshot failed. Condition: Snapshot failed due to timeout",
                result.error_reason,
            )
        logging.info("Finished test_snapshot_watch_failure_condition.")

    def test_snapshot_watch_error_event(self):
        """Test snapshot failure on 'ERROR' event type."""
        logging.info("Starting test_snapshot_watch_error_event...")
        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True

        mock_watch = MagicMock()
        with patch(
            "k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch"
        ) as mock_watch_cls:
            mock_watch_cls.return_value = mock_watch
            error_event = {
                "type": "ERROR",
                "object": {"code": 500, "message": "Internal Server Error"},
            }
            mock_watch.stream.return_value = [error_event]

            self.client.custom_objects_api.create_namespaced_custom_object.return_value = {
                "metadata": {"resourceVersion": "100"}
            }

            result = self.client.snapshot("test-trigger-error")

            self.assertFalse(result.success)
            self.assertEqual(result.error_code, 1)
            self.assertIn("Snapshot watch error:", result.error_reason)
        logging.info("Finished test_snapshot_watch_error_event.")

    def test_snapshot_watch_deleted_event(self):
        """Test snapshot failure on 'DELETED' event type."""
        logging.info("Starting test_snapshot_watch_deleted_event...")
        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True

        mock_watch = MagicMock()
        with patch(
            "k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch"
        ) as mock_watch_cls:
            mock_watch_cls.return_value = mock_watch
            deleted_event = {"type": "DELETED", "object": {}}
            mock_watch.stream.return_value = [deleted_event]

            self.client.custom_objects_api.create_namespaced_custom_object.return_value = {
                "metadata": {"resourceVersion": "100"}
            }

            result = self.client.snapshot("test-trigger-deleted")

            self.assertFalse(result.success)
            self.assertEqual(result.error_code, 1)
            self.assertIn("was deleted", result.error_reason)
        logging.info("Finished test_snapshot_watch_deleted_event.")

    def test_snapshot_watch_generic_exception(self):
        """Test snapshot failure on generic exception during watch."""
        logging.info("Starting test_snapshot_watch_generic_exception...")
        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True

        mock_watch = MagicMock()
        with patch(
            "k8s_agent_sandbox.gke_extensions.podsnapshot_client.watch.Watch"
        ) as mock_watch_cls:
            mock_watch_cls.return_value = mock_watch
            # Simulate generic exception
            mock_watch.stream.side_effect = Exception("Something went wrong")

            self.client.custom_objects_api.create_namespaced_custom_object.return_value = {
                "metadata": {"resourceVersion": "100"}
            }

            result = self.client.snapshot("test-trigger-generic")

            self.assertFalse(result.success)
            self.assertEqual(result.error_code, 1)
            self.assertIn("Unexpected error: Something went wrong", result.error_reason)
        logging.info("Finished test_snapshot_watch_generic_exception.")

    def test_snapshot_invalid_name_api_exception(self):
        """Test snapshot failure when trigger name is invalid (ApiException)."""
        logging.info("Starting test_snapshot_invalid_name_api_exception...")
        self.client.pod_name = "test-pod"
        self.client.snapshot_crd_installed = True

        self.client.custom_objects_api.create_namespaced_custom_object.side_effect = ApiException(
            status=400,
            reason="BadRequest",
            http_resp=MagicMock(
                data='Invalid value: "Test_Trigger": must be a lowercase RFC 1123 subdomain'
            ),
        )

        result = self.client.snapshot("Test_Trigger")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Failed to create PodSnapshotManualTrigger", result.error_reason)
        self.assertIn("Invalid value", result.error_reason)
        logging.info("Finished test_snapshot_invalid_name_api_exception.")


if __name__ == "__main__":
    unittest.main()
