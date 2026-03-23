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

from k8s_agent_sandbox.gke_extensions.snapshots.sandbox_with_snapshot_support import SandboxWithSnapshotSupport
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
    POD_NAME_ANNOTATION,
)

logger = logging.getLogger(__name__)

class TestSandboxWithSnapshotSupport(unittest.TestCase):
    @patch('k8s_agent_sandbox.sandbox.SandboxConnector')
    @patch('k8s_agent_sandbox.sandbox.create_tracer_manager')
    @patch('k8s_agent_sandbox.sandbox.CommandExecutor')
    @patch('k8s_agent_sandbox.sandbox.Filesystem')
    def setUp(self, mock_fs, mock_ce, mock_ctm, mock_conn):
        mock_ctm.return_value = (None, None)
        
        self.mock_k8s_helper = MagicMock()

        # Create SandboxWithSnapshotSupport
        self.sandbox = SandboxWithSnapshotSupport(
            sandbox_id="test-sandbox",
            namespace="test-ns",
            k8s_helper=self.mock_k8s_helper,
            pod_name="test-pod"
        )
        # Access the underlying engine
        self.engine = self.sandbox.snapshots

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
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.return_value = mock_created_obj

        result = self.engine.create("test-trigger")

        self.assertEqual(result.error_code, 0)
        self.assertTrue(result.success)
        self.assertEqual(result.snapshot_uid, "snapshot-uid")
        self.assertIn("test-trigger", result.trigger_name)

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.assert_called_once()
        _, kwargs = self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.call_args
        self.assertEqual(kwargs['group'], PODSNAPSHOT_API_GROUP)
        self.assertEqual(kwargs['body']['spec']['targetPod'], "test-pod")

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
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.side_effect = ApiException("Create failed")

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
        self.assertIn("Snapshot failed. Condition: Snapshot failed due to timeout", result.error_reason)

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
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.side_effect = ApiException("Invalid value: 'Test_Trigger'")

        result = self.engine.create("Test_Trigger")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, 1)
        self.assertIn("Failed to create PodSnapshotManualTrigger", result.error_reason)
        self.assertIn("Invalid value", result.error_reason)

    def test_delete_manual_triggers(self):
        self.engine.created_manual_triggers = ["trigger-1", "trigger-2"]

        self.engine.delete_manual_triggers()

        self.assertEqual(
            self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.call_count, 2
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

if __name__ == "__main__":
    unittest.main()
