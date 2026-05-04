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
from kubernetes.client import ApiException

from k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support import (
    SandboxWithSnapshotSupport,
    SUCCESS_CODE,
    ERROR_CODE,
    SuspendResponse,
    ResumeResponse,
)
from k8s_agent_sandbox.constants import (
    SANDBOX_NAME_HASH_LABEL,
    PODSNAPSHOT_POD_NAME_ANNOTATION,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
    POD_NAME_ANNOTATION,
    PODSNAPSHOT_PLURAL,
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
)
from k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine import (
    ListSnapshotResult,
    SnapshotDetail,
    DeleteSnapshotResult,
    SnapshotResponse,
)

logger = logging.getLogger(__name__)


class TestSandboxWithSnapshotSupport(unittest.TestCase):
    @patch("k8s_agent_sandbox.sandbox.SandboxConnector")
    @patch("k8s_agent_sandbox.sandbox.create_tracer_manager")
    @patch("k8s_agent_sandbox.sandbox.CommandExecutor")
    @patch("k8s_agent_sandbox.sandbox.Filesystem")
    def setUp(self, mock_fs, mock_ce, mock_ctm, mock_conn):
        logging.info(f"Starting {self._testMethodName}...")
        mock_ctm.return_value = (None, None)

        self.mock_k8s_helper = MagicMock()

        # Create SandboxWithSnapshotSupport
        self.sandbox = SandboxWithSnapshotSupport(
            namespace="test-ns",
            k8s_helper=self.mock_k8s_helper,
            claim_name="test-claim",
            sandbox_id="test-id",
        )
        self.sandbox.get_pod_name = MagicMock(return_value="test-pod")
        # Access the underlying engine
        self.engine = self.sandbox.snapshots
        self.engine.get_pod_name_func = self.sandbox.get_pod_name
        
        self.engine.get_sandbox_name_hash_func = MagicMock(return_value="test-hash")

    def tearDown(self):
        logging.info(f"Finished {self._testMethodName}.")

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_success(self, mock_watch_cls):
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

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

        mock_created_obj = {"metadata": {"resourceVersion": "123"}, "status": {}}
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = (
            mock_created_obj
        )

        result = self.engine.create("test-trigger")

        self.sandbox.get_pod_name.assert_called_once()
        self.assertEqual(result.error_code, SUCCESS_CODE)
        self.assertTrue(result.success)
        self.assertEqual(result.snapshot_uid, "snapshot-uid")
        self.assertEqual(result.snapshot_timestamp, "2023-01-01T00:00:00Z")
        self.assertIn("test-trigger", result.trigger_name)

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.assert_called_once()
        _, kwargs = (
            self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.call_args
        )
        self.assertEqual(kwargs["group"], PODSNAPSHOT_API_GROUP)
        self.assertEqual(kwargs["body"]["spec"]["targetPod"], "test-pod")

        mock_watch.stream.assert_called_once()
        _, stream_kwargs = mock_watch.stream.call_args
        self.assertEqual(stream_kwargs.get("resource_version"), "123")

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_processed_retry(self, mock_watch_cls):
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        event_incomplete = {
            "type": "MODIFIED",
            "object": {
                "status": {
                    "conditions": [
                        {
                            "type": "Triggered",
                            "status": "False",
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

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = {
            "metadata": {"resourceVersion": "999"}
        }

        result = self.engine.create("test-retry")
        self.assertTrue(result.success)
        self.assertEqual(result.snapshot_uid, "snapshot-uid-retry")

    def test_snapshots_create_api_exception(self):
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.side_effect = ApiException(
            "Create failed"
        )

        result = self.engine.create("test-trigger")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Failed to create PodSnapshotManualTrigger", result.error_reason)

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_timeout(self, mock_watch_cls):
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch
        mock_watch.stream.return_value = []

        result = self.engine.create("test-trigger", podsnapshot_timeout=1)

        self.assertEqual(result.error_code, 1)
        self.assertFalse(result.success)
        self.assertIn("timed out", result.error_reason)

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_watch_failure(self, mock_watch_cls):
        mock_watch = MagicMock()
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
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = {
            "metadata": {"resourceVersion": "100"}
        }

        result = self.engine.create("test-trigger-fail")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn(
            "Snapshot failed. Condition: Snapshot failed due to timeout",
            result.error_reason,
        )

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_watch_error(self, mock_watch_cls):
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch
        error_event = {
            "type": "ERROR",
            "object": {"code": 500, "message": "Internal Server Error"},
        }
        mock_watch.stream.return_value = [error_event]
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = {
            "metadata": {"resourceVersion": "100"}
        }

        result = self.engine.create("test-trigger-error")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Snapshot watch error:", result.error_reason)

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_watch_deleted(self, mock_watch_cls):
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch
        deleted_event = {"type": "DELETED", "object": {}}
        mock_watch.stream.return_value = [deleted_event]
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = {
            "metadata": {"resourceVersion": "100"}
        }

        result = self.engine.create("test-trigger-deleted")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("was deleted", result.error_reason)

    @patch("k8s_agent_sandbox.gke_extensions.snapshots.utils.watch.Watch")
    def test_snapshots_create_generic_exception(self, mock_watch_cls):
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch
        mock_watch.stream.side_effect = Exception("Something went wrong")
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = {
            "metadata": {"resourceVersion": "100"}
        }

        result = self.engine.create("test-trigger-generic")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Server error: Something went wrong", result.error_reason)

    def test_snapshots_create_invalid_name(self):
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.side_effect = ApiException(
            "Invalid value: 'Test_Trigger'"
        )

        result = self.engine.create("Test_Trigger")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Failed to create PodSnapshotManualTrigger", result.error_reason)
        self.assertIn("Invalid value", result.error_reason)

    def test_delete_manual_triggers(self):
        self.engine.created_manual_triggers = ["trigger-1", "trigger-2"]

        self.engine.delete_manual_triggers()

        self.assertEqual(
            self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.call_count,
            2,
        )

        calls = [
            call(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.sandbox.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                name="trigger-1",
            ),
            call(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.sandbox.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                name="trigger-2",
            ),
        ]
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.assert_has_calls(
            calls, any_order=True
        )
        self.assertEqual(len(self.engine.created_manual_triggers), 0)

    def test_is_restored_from_snapshot_success(self):
        """Test successful identification of restore from snapshot."""
        mock_pod = MagicMock()
        mock_condition = MagicMock()
        mock_condition.type = "PodRestored"
        mock_condition.status = "True"
        mock_condition.message = "Restored from snapshot test-uid"
        mock_pod.status.conditions = [mock_condition]

        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.sandbox._is_restored_from_snapshot("test-uid")

        self.assertTrue(result.success, result.error_reason)
        self.assertEqual(result.error_code, SUCCESS_CODE)
        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.assert_called_once_with(
            "test-pod", "test-ns"
        )

    def test_is_restored_from_snapshot_empty_uid(self):
        """Test is_restored_from_snapshot with empty UID."""
        result = self.sandbox._is_restored_from_snapshot("")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Snapshot UID cannot be empty", result.error_reason)

    def test_is_restored_from_snapshot_pending_or_failed(self):
        """Test is_restored_from_snapshot when PodRestored condition is not True."""
        mock_pod = MagicMock()
        mock_condition = MagicMock()
        mock_condition.type = "PodRestored"
        mock_condition.status = "False"
        mock_condition.reason = "FailedToRestore"
        mock_condition.message = "Snapshot not found"
        mock_pod.status.conditions = [mock_condition]

        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.sandbox._is_restored_from_snapshot("test-uid")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Restore attempted but pending or failed", result.error_reason)
        self.assertIn("status: 'False'", result.error_reason)
        self.assertIn("reason: 'FailedToRestore'", result.error_reason)
        self.assertIn("message: 'Snapshot not found'", result.error_reason)

    def test_is_restored_from_snapshot_no_pod_name(self):
        """Test is_restored_from_snapshot when pod name is missing."""
        self.sandbox.get_pod_name.return_value = None
        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Pod name not found", result.error_reason)

    def test_is_restored_from_snapshot_no_status(self):
        """Test is_restored_from_snapshot when pod status is None."""
        mock_pod = MagicMock()
        mock_pod.status = None
        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Pod status or conditions not found", result.error_reason)

    def test_is_restored_from_snapshot_no_conditions(self):
        """Test is_restored_from_snapshot when pod has no conditions."""
        mock_pod = MagicMock()
        mock_pod.status.conditions = None
        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Pod status or conditions not found", result.error_reason)

    def test_is_restored_from_snapshot_wrong_uid(self):
        """Test is_restored_from_snapshot when restored from a different snapshot."""
        mock_pod = MagicMock()
        mock_condition = MagicMock()
        mock_condition.type = "PodRestored"
        mock_condition.status = "True"
        mock_condition.message = "Restored from snapshot other-uid"
        mock_pod.status.conditions = [mock_condition]

        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("not restored from the given snapshot", result.error_reason)

    def test_is_restored_from_snapshot_not_restored(self):
        """Test is_restored_from_snapshot when not restored from any snapshot."""
        mock_pod = MagicMock()
        mock_condition = MagicMock()
        mock_condition.type = "PodScheduled"
        mock_condition.status = "True"
        mock_pod.status.conditions = [mock_condition]

        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod

        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("started as a fresh instance", result.error_reason)

    def test_is_restored_from_snapshot_api_exception(self):
        """Test is_restored_from_snapshot handling ApiException."""
        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.side_effect = ApiException(
            status=500, reason="Internal Server Error"
        )

        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Failed to check pod restore status", result.error_reason)

    def test_is_restored_from_snapshot_generic_exception(self):
        """
        Test is_restored_from_snapshot handling generic exception.
        A generic exception here could represent unexpected errors such as:
        - Network issues leading to aborted connections or timeouts (urllib3.exceptions or socket errors)
        - Deserialization issues when parsing the API response (e.g. ValueError or TypeError)
        - Threading/Async context errors within the underlying kubernetes client library
        """
        self.mock_k8s_helper.core_v1_api.read_namespaced_pod.side_effect = ValueError(
            "Deserialization error"
        )

        result = self.sandbox._is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Unexpected error", result.error_reason)

    def test_snapshots_list_success(self):
        """Test list snapshots returning properly formatted objects."""
        mock_response = {
            "items": [
                {
                    "metadata": {
                        "name": "snap-1",
                        "uid": "uid-1",
                        "creationTimestamp": "2023-01-02T00:00:00Z",
                        "labels": {SANDBOX_NAME_HASH_LABEL: "test-hash"},
                        "annotations": {PODSNAPSHOT_POD_NAME_ANNOTATION: "test-pod"},
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {
                        "name": "snap-2",
                        "uid": "uid-2",
                        "creationTimestamp": "2023-01-01T00:00:00Z",
                        "labels": {SANDBOX_NAME_HASH_LABEL: "test-hash"},
                        "annotations": {PODSNAPSHOT_POD_NAME_ANNOTATION: "test-pod"},
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {
                        "name": "snap-not-ready",
                        "uid": "uid-3",
                        "creationTimestamp": "2023-01-03T00:00:00Z",
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "False"}]},
                },
            ]
        }
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.return_value = (
            mock_response
        )

        result = self.engine.list()

        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 2)
        # Verify it sorted by creationTimestamp newest first
        self.assertEqual(result.snapshots[0].snapshot_uid, "snap-1")
        self.assertEqual(result.snapshots[0].source_pod, "test-pod")
        self.assertEqual(result.snapshots[1].snapshot_uid, "snap-2")
        self.assertEqual(result.snapshots[1].source_pod, "test-pod")
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.assert_called_once_with(
            group=PODSNAPSHOT_API_GROUP,
            version=PODSNAPSHOT_API_VERSION,
            namespace="test-ns",
            plural=PODSNAPSHOT_PLURAL,
            label_selector=f"{SANDBOX_NAME_HASH_LABEL}=test-hash",
        )

    def test_snapshots_list_filter_empty(self):
        """Test list snapshots with filter_by={} includes non-ready snapshots."""
        mock_response = {
            "items": [
                {
                    "metadata": {
                        "name": "ready-snap",
                        "uid": "uid1",
                        "creationTimestamp": "2023-01-01T00:00:00Z",
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {
                        "name": "not-ready-snap",
                        "uid": "uid2",
                        "creationTimestamp": "2023-01-02T00:00:00Z",
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "False"}]},
                },
            ]
        }
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.return_value = (
            mock_response
        )

        result = self.engine.list(filter_by={"ready_only": False})
        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 2)
        # Sorted by creationTimestamp descending
        self.assertEqual(result.snapshots[0].snapshot_uid, "not-ready-snap")
        self.assertEqual(result.snapshots[1].snapshot_uid, "ready-snap")

    def test_snapshots_list_filter_incorrect_arguments(self):
        """Test list snapshots with a incorrect arguments for filter_by."""
        mock_response = {
            "items": [
                {
                    "metadata": {
                        "name": "ready-snap",
                        "uid": "uid1",
                        "creationTimestamp": "2023-01-01T00:00:00Z",
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {
                        "name": "not-ready-snap",
                        "uid": "uid2",
                        "creationTimestamp": "2023-01-02T00:00:00Z",
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "False"}]},
                },
            ]
        }
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.return_value = (
            mock_response
        )

        # Passing a random dict should fail because extra fields are forbidden.
        result = self.engine.list(filter_by={"random_key": "random_value"})
        self.assertFalse(result.success)
        self.assertEqual(len(result.snapshots), 0)
        self.assertIn("Invalid filter parameters", result.error_reason)

    def test_snapshots_list_none_timestamp(self):
        """Test list snapshots doesn't crash when creationTimestamp is None."""
        mock_response = {
            "items": [
                {
                    "metadata": {
                        "name": "snap-1",
                        "uid": "uid-1",
                        "creationTimestamp": None,  # Test Case: None
                        "labels": {SANDBOX_NAME_HASH_LABEL: "test-hash"},
                        "annotations": {PODSNAPSHOT_POD_NAME_ANNOTATION: "test-pod"},
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {
                        "name": "snap-2",
                        "uid": "uid-2",
                        "creationTimestamp": "2023-01-01T00:00:00Z",
                        "labels": {SANDBOX_NAME_HASH_LABEL: "test-hash"},
                        "annotations": {PODSNAPSHOT_POD_NAME_ANNOTATION: "test-pod"},
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
            ]
        }
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.return_value = (
            mock_response
        )

        result = self.engine.list()

        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 2)
        # Verify it sorted correctly even with None (None/empty string should come last in reverse sort)
        self.assertEqual(result.snapshots[0].snapshot_uid, "snap-2")
        self.assertEqual(result.snapshots[1].snapshot_uid, "snap-1")

    def test_snapshots_list_no_results(self):
        """Test list snapshots returns successfully with empty list if none found."""
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.return_value = {
            "items": []
        }
        result = self.engine.list()
        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 0)
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.assert_called_once_with(
            group=PODSNAPSHOT_API_GROUP,
            version=PODSNAPSHOT_API_VERSION,
            namespace="test-ns",
            plural=PODSNAPSHOT_PLURAL,
            label_selector=f"{SANDBOX_NAME_HASH_LABEL}=test-hash",
        )

    def test_snapshots_list_no_pod_name(self):
        """Test list snapshots fails when pod name is missing."""
        self.sandbox.get_pod_name.return_value = None
        result = self.engine.list()
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Pod name not found", result.error_reason)

    def test_snapshots_list_no_sandbox_name_hash(self):
        """Test list snapshots fails when sandbox name hash is missing."""
        self.engine.get_sandbox_name_hash_func.return_value = None
        result = self.engine.list()
        self.assertFalse(result.success)
        self.assertEqual(result.error_code, ERROR_CODE)
        self.assertIn("Sandbox name hash not found", result.error_reason)

    def test_snapshots_list_api_exception(self):
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.side_effect = ApiException(
            500, "Internal Server Error"
        )
        result = self.engine.list()
        self.assertFalse(result.success)
        self.assertIn("Failed to list PodSnapshots", result.error_reason)

    def test_snapshots_list_generic_exception(self):
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object.side_effect = ValueError(
            "Unexpected"
        )
        result = self.engine.list()
        self.assertFalse(result.success)
        self.assertIn("Unexpected error", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_uid_provided(self, mock_wait):
        """Test delete snapshots when a specific snapshot UID is provided."""
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.return_value = (
            {}
        )

        result = self.engine.delete(snapshot_uid="target-snap")
        self.assertTrue(result.success)
        self.assertEqual(result.deleted_snapshots, ["target-snap"])
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.assert_called_once_with(
            group=PODSNAPSHOT_API_GROUP,
            version=PODSNAPSHOT_API_VERSION,
            namespace="test-ns",
            plural=PODSNAPSHOT_PLURAL,
            name="target-snap",
        )
        mock_wait.assert_called_once_with(
            k8s_helper=self.mock_k8s_helper,
            namespace="test-ns",
            snapshot_uid="target-snap",
            resource_version=None,
            timeout=180,
        )

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_with_list(self, mock_wait):
        """Test delete snapshots fetching list of snapshots when uid is not provided."""

        with patch.object(self.engine, "list") as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="snap-a",
                        source_pod="test-pod",
                        creation_timestamp="2023-01-01T00:00:00Z",
                        status="Ready",
                    )
                ],
                error_reason="",
                error_code=SUCCESS_CODE,
            )
            self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.return_value = (
                {}
            )

            result = self.engine.delete_all()

            self.assertTrue(result.success)
            self.assertEqual(result.deleted_snapshots, ["snap-a"])
            mock_list.assert_called_once_with(
                filter_by={"ready_only": False}
            )
            self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.assert_called_once_with(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace="test-ns",
                plural=PODSNAPSHOT_PLURAL,
                name="snap-a",
            )
            mock_wait.assert_called_once_with(
                k8s_helper=self.mock_k8s_helper,
                namespace="test-ns",
                snapshot_uid="snap-a",
                resource_version=None,
                timeout=180,
            )

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_api_exception(self, mock_wait):
        """Test delete snapshots gracefully handling failure on one of the items."""
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.side_effect = ApiException(
            500, "Internal error"
        )
        result = self.engine.delete(snapshot_uid="target-snap")
        self.assertFalse(result.success)
        self.assertEqual(result.deleted_snapshots, [])
        self.assertIn("Failed to delete PodSnapshot", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_partial_failure(self, mock_wait):
        """Test delete snapshots continuing loop and aggregating errors on partial failure."""

        # Mock list to return 3 snapshots
        with patch.object(self.engine, "list") as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="snap-1",
                        source_pod="pod",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                    SnapshotDetail(
                        snapshot_uid="snap-2",
                        source_pod="pod",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                    SnapshotDetail(
                        snapshot_uid="snap-3",
                        source_pod="pod",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                ],
                error_reason="",
                error_code=0,
            )

            # Mock delete calls:
            # snap-1: Success
            # snap-2: ApiException (500)
            # snap-3: Success
            def mock_delete(group, version, namespace, plural, name):
                if name == "snap-2":
                    raise ApiException(500, "Internal error")
                return {}

            self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.side_effect = (
                mock_delete
            )

            result = self.engine.delete_all()

            self.assertFalse(result.success)
            self.assertEqual(result.deleted_snapshots, ["snap-1", "snap-3"])
            self.assertIn("Failed to delete PodSnapshot 'snap-2'", result.error_reason)
            self.assertEqual(
                self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.call_count,
                3,
            )
            # Verify wait was called for successful deletions
            self.assertEqual(mock_wait.call_count, 2)
            mock_wait.assert_has_calls(
                [
                    call(
                        k8s_helper=self.mock_k8s_helper,
                        namespace="test-ns",
                        snapshot_uid="snap-1",
                        resource_version=None,
                        timeout=180,
                    ),
                    call(
                        k8s_helper=self.mock_k8s_helper,
                        namespace="test-ns",
                        snapshot_uid="snap-3",
                        resource_version=None,
                        timeout=180,
                    ),
                ],
                any_order=True,
            )

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_generic_exception(self, mock_wait):
        """Test delete snapshots handling generic Exception during deletion."""
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.side_effect = Exception(
            "Generic error"
        )

        result = self.engine.delete(snapshot_uid="target-snap")
        self.assertFalse(result.success)
        self.assertIn("Unexpected error deleting PodSnapshot", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_api_exception_404(self, mock_wait):
        """Test delete snapshots interpreting 404 as successful (already deleted)."""
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.side_effect = ApiException(
            404, "Not Found"
        )
        result = self.engine.delete(snapshot_uid="target-snap")
        self.assertTrue(result.success)
        self.assertEqual(result.deleted_snapshots, [])
        mock_wait.assert_not_called()

    def test_snapshots_delete_list_fail(self):
        """Test delete snapshots returning early false if list query fails."""
        with patch.object(self.engine, "list") as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason="Could not connect",
                error_code=ERROR_CODE,
            )
            result = self.engine.delete_all()
            self.assertFalse(result.success)
            self.assertIn(
                "Failed to list snapshots before deletion", result.error_reason
            )
            self.assertEqual(result.deleted_snapshots, [])

    def test_snapshots_delete_all(self):
        """Test delete_all calls _execute_deletion with scope='global'."""
        with patch.object(self.engine, "_execute_deletion") as mock_execute:
            mock_execute.return_value = DeleteSnapshotResult(
                success=True,
                deleted_snapshots=["snap-x"],
                error_reason="",
                error_code=0,
            )
            self.engine.delete_all()
            mock_execute.assert_called_once_with(scope="global", timeout=180)



    def test_snapshots_delete_empty_fails(self):
        """Test delete raises TypeError if snapshot_uid is missing."""
        with self.assertRaises(TypeError):
            self.engine.delete()

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine.wait_for_snapshot_deletion"
    )
    def test_snapshots_delete_timeout(self, mock_wait):
        """Test delete snapshots handling timeout in wait."""
        mock_wait.return_value = False

        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.return_value = (
            {}
        )

        result = self.engine.delete(snapshot_uid="target-snap")

        self.assertFalse(result.success)
        self.assertEqual(result.deleted_snapshots, [])
        self.assertIn("Timed out waiting for confirmation", result.error_reason)

    def test_snapshots_delete_all_no_snapshots_found(self):
        """Test delete_all returns success when no snapshots are found."""
        with patch.object(self.engine, "list") as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[],
                error_reason="",
                error_code=0,
            )

            result = self.engine.delete_all()

            self.assertTrue(result.success)
            self.assertEqual(result.deleted_snapshots, [])
            
    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_termination')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=False)
    def test_suspend_success(self, mock_is_suspended, mock_wait):
        """Test suspend successfully takes a snapshot and scales down."""
        mock_wait.return_value = True
        with patch.object(self.engine, 'create') as mock_create:
            mock_create.return_value = SnapshotResponse(
                success=True, trigger_name="test-trigger", snapshot_uid="uid-123",
                snapshot_timestamp="2023-01-01T00:00:00Z", error_reason="", error_code=0
            )
            
            result = self.sandbox.suspend(snapshot_before_suspend=True)
            
            self.assertTrue(result.success)
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.assert_called_once_with(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=self.sandbox.namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=self.sandbox.sandbox_id,
                body={"spec": {"replicas": 0}}
            )

    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_termination')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=False)
    def test_suspend_without_snapshot(self, mock_is_suspended, mock_wait):
        """Test suspend successfully scales down without taking a snapshot."""
        mock_wait.return_value = True
        result = self.sandbox.suspend(snapshot_before_suspend=False)
        
        self.assertTrue(result.success)
        self.assertIsNone(result.snapshot_response)
        self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.assert_called_once()

    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=False)
    def test_suspend_pod_not_there(self, mock_is_suspended):
        """Scenario 1: What happens if pod is not there when suspending."""
        # If the pod is not there, the snapshot creation fails.
        # We simulate the snapshot failure and assert that the scale down does not occur.
        with patch.object(self.engine, 'create') as mock_create:
            mock_create.return_value = SnapshotResponse(
                success=False, trigger_name="test-trigger", snapshot_uid=None,
                snapshot_timestamp=None, error_reason="Pod not found", error_code=1
            )
            
            result = self.sandbox.suspend(snapshot_before_suspend=True)
            
            self.assertFalse(result.success)
            self.assertIn("Pod not found", result.error_reason)
            # Ensure we don't scale down the replica state if the snapshot fails
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.assert_not_called()

    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_ready')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended')
    def test_resume_pod_already_exists(self, mock_is_suspended, mock_wait):
        """Scenario 2: What happens if pod is there before resume call."""
        mock_wait.return_value = True
        mock_is_suspended.return_value = True
        with patch.object(self.engine, 'list') as mock_list:
            mock_list.return_value = ListSnapshotResult(success=True, snapshots=[], error_reason="", error_code=0)
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.return_value = {"status": "patched"}
            
            result = self.sandbox.resume()
            
            self.assertTrue(result.success)
            self.assertFalse(result.restored_from_snapshot)
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.assert_called_once_with(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=self.sandbox.namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=self.sandbox.sandbox_id,
                body={"spec": {"replicas": 1}}
            )

    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_termination')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended')
    def test_suspend_multiple_calls(self, mock_is_suspended, mock_wait):
        """Scenario 3: What happens when multiple calls are made to suspend before resuming."""
        mock_wait.return_value = True
        mock_is_suspended.side_effect = [False, True]
        with patch.object(self.engine, 'create') as mock_create:
            mock_create.return_value = SnapshotResponse(
                success=True, trigger_name="test-trigger", snapshot_uid="uid-123",
                snapshot_timestamp="2023-01-01T00:00:00Z", error_reason="", error_code=0
            )
            
            self.sandbox.suspend(snapshot_before_suspend=True)
            self.sandbox.suspend(snapshot_before_suspend=True)
            
            # Suspend APIs are called only
            # Second suspend returns success early
            self.assertEqual(mock_create.call_count, 1)
            self.assertEqual(
                self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.call_count, 1
            )

    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_ready')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended')
    def test_resume_not_restored_from_snapshot(self, mock_is_suspended, mock_wait):
        """Scenario 4: What happens if the pod is not restored from snapshot on resume."""
        mock_wait.return_value = True
        mock_is_suspended.return_value = True
        with patch.object(self.engine, 'list') as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True, 
                snapshots=[SnapshotDetail(snapshot_uid="uid-123", source_pod="p", creation_timestamp="ts", status="Ready")], 
                error_reason="", error_code=0
            )
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.return_value = {"status": "patched"}
            
            mock_pod = MagicMock()
            mock_condition_ready = MagicMock()
            mock_condition_ready.type = "Ready"
            mock_condition_ready.status = "True"
            mock_pod.status.conditions = [mock_condition_ready]
            mock_pod.metadata.deletion_timestamp = None
            self.mock_k8s_helper.core_v1_api.read_namespaced_pod.return_value = mock_pod
            
            result = self.sandbox.resume(wait_timeout=2)
            
            self.assertFalse(result.success)
            self.assertFalse(result.restored_from_snapshot)
            self.assertIn("started as a fresh instance", result.error_reason)
        
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=False)
    def test_suspend_api_exception(self, mock_is_suspended):
        """Test suspend raises exception when custom object patch API call fails."""
        with patch.object(self.engine, 'create') as mock_create:
            mock_create.return_value = SnapshotResponse(
                success=True, trigger_name="test-trigger", snapshot_uid="uid-123",
                snapshot_timestamp="2023-01-01T00:00:00Z", error_reason="", error_code=0
            )
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.side_effect = ApiException("Failed")
            
            result = self.sandbox.suspend()
            self.assertFalse(result.success)
            self.assertIn("Failed", result.error_reason)

    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=True)
    def test_resume_api_exception(self, mock_is_suspended):
        """Test resume raises exception when custom object patch API call fails."""
        with patch.object(self.sandbox, '_get_latest_snapshot_uid', return_value='uid-123'):
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.side_effect = ApiException("Failed")
            
            result = self.sandbox.resume()
            self.assertFalse(result.success)
            self.assertIn("Failed", result.error_reason)

    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=True)
    def test_resume_get_snapshot_uid_failure(self, mock_is_suspended):
        """Test resume handles failure when retrieving latest snapshot UID."""
        with patch.object(self.sandbox, '_get_latest_snapshot_uid', side_effect=RuntimeError("List error")):
            result = self.sandbox.resume()
            self.assertFalse(result.success)
            self.assertIn("Failed to get latest snapshot UID: List error", result.error_reason)
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.assert_not_called()

    def test_get_latest_snapshot_uid_list_failure(self):
        """Test _get_latest_snapshot_uid raises RuntimeError when list fails."""
        with patch.object(self.engine, 'list') as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=False, snapshots=[], error_reason="List failed", error_code=1
            )
            with self.assertRaises(RuntimeError) as context:
                self.sandbox._get_latest_snapshot_uid()
            self.assertIn("Snapshot list request failed: List failed", str(context.exception))

    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_ready')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=True)
    def test_resume_timeout(self, mock_is_suspended, mock_wait):
        """Test resume times out when wait_for_pod_ready expires."""
        mock_wait.return_value = False
        with patch.object(self.engine, 'list') as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True, 
                snapshots=[SnapshotDetail(snapshot_uid="uid-123", source_pod="p", creation_timestamp="ts", status="Ready")], 
                error_reason="", error_code=0
            )
            self.mock_k8s_helper.custom_objects_api.patch_namespaced_custom_object.return_value = {"status": "patched"}
            
            result = self.sandbox.resume(wait_timeout=1)
            
            self.assertFalse(result.success)
            self.assertFalse(result.restored_from_snapshot)
            self.assertEqual(result.snapshot_uid, "uid-123")
            self.assertIn("Timed out", result.error_reason)

    @patch('k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support.wait_for_pod_termination')
    @patch.object(SandboxWithSnapshotSupport, 'is_suspended', return_value=False)
    def test_suspend_timeout(self, mock_is_suspended, mock_wait):
        """Test suspend times out when wait_for_pod_termination expires."""
        mock_wait.return_value = False
        with patch.object(self.engine, 'create') as mock_create:
            mock_create.return_value = SnapshotResponse(
                success=True, trigger_name="test-trigger", snapshot_uid="uid-123",
                snapshot_timestamp="2023-01-01T00:00:00Z", error_reason="", error_code=0
            )
            
            result = self.sandbox.suspend(wait_timeout=1)
            
            self.assertFalse(result.success)
            self.assertIn("Timed out", result.error_reason)


    def test_is_suspended_true(self):
        self.mock_k8s_helper.custom_objects_api.get_namespaced_custom_object.return_value = {
            "spec": {"replicas": 0},
            "status": {}
        }
        self.assertTrue(self.sandbox.is_suspended())

    def test_is_suspended_false(self):
        self.mock_k8s_helper.custom_objects_api.get_namespaced_custom_object.return_value = {
            "spec": {"replicas": 1},
            "status": {"podIPs": ["10.0.0.1"]}
        }
        self.assertFalse(self.sandbox.is_suspended())

    def test_get_sandbox_name_hash_success(self):
        """Test get_sandbox_name_hash reads from status.selector and caches it."""
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {"selector": f"{SANDBOX_NAME_HASH_LABEL}=test-hash-value"}
        }
        
        # First call should fetch from K8s
        res1 = self.sandbox.get_sandbox_name_hash()
        self.assertEqual(res1, "test-hash-value")
        self.mock_k8s_helper.get_sandbox.assert_called_once_with("test-id", "test-ns")
        
        # Second call should use cache
        self.mock_k8s_helper.get_sandbox.reset_mock()
        res2 = self.sandbox.get_sandbox_name_hash()
        self.assertEqual(res2, "test-hash-value")
        self.mock_k8s_helper.get_sandbox.assert_not_called()

    def test_get_sandbox_name_hash_not_found(self):
        """Test get_sandbox_name_hash returns None when not found."""
        self.mock_k8s_helper.get_sandbox.return_value = {}
        
        res = self.sandbox.get_sandbox_name_hash()
        self.assertIsNone(res)

if __name__ == "__main__":
    unittest.main()
