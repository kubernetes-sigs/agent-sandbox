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
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

pytest.importorskip("kubernetes_asyncio")

from kubernetes_asyncio.client import ApiException

from k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine import (
    AsyncSnapshotEngine,
)
from k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine import (
    SNAPSHOT_SUCCESS_CODE,
    SNAPSHOT_ERROR_CODE,
    ListSnapshotResult,
    SnapshotDetail,
    DeleteSnapshotResult,
)
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_POD_NAME_LABEL,
)


class TestAsyncSnapshotEngine(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.mock_k8s_helper = MagicMock()
        self.mock_k8s_helper._ensure_initialized = AsyncMock()
        self.mock_k8s_helper.custom_objects_api = MagicMock()
        self.mock_k8s_helper.core_v1_api = MagicMock()

        self.get_pod_name_func = AsyncMock(return_value="test-pod")

        self.engine = AsyncSnapshotEngine(
            namespace="test-ns",
            k8s_helper=self.mock_k8s_helper,
            get_pod_name_func=self.get_pod_name_func,
        )

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_to_be_completed"
    )
    async def test_create_success(self, mock_wait):
        mock_result = MagicMock()
        mock_result.snapshot_uid = "snapshot-uid"
        mock_result.snapshot_timestamp = "2023-01-01T00:00:00Z"
        mock_wait.return_value = mock_result

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(return_value={"metadata": {"resourceVersion": "123"}})
        )

        result = await self.engine.create("test-trigger")

        self.assertTrue(result.success)
        self.assertEqual(result.snapshot_uid, "snapshot-uid")
        self.assertEqual(result.snapshot_timestamp, "2023-01-01T00:00:00Z")
        self.assertIn("test-trigger", result.trigger_name)
        self.assertEqual(result.error_code, SNAPSHOT_SUCCESS_CODE)

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object.assert_called_once()
        self.get_pod_name_func.assert_called_once()

    async def test_create_api_exception(self):
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(side_effect=ApiException("Create failed"))
        )

        result = await self.engine.create("test-trigger")

        self.assertFalse(result.success)
        self.assertEqual(result.error_code, SNAPSHOT_ERROR_CODE)
        self.assertIn("Failed to create PodSnapshotManualTrigger", result.error_reason)

    async def test_create_api_exception_403_rbac_hint(self):
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(side_effect=ApiException(status=403))
        )

        result = await self.engine.create("test-trigger")

        self.assertFalse(result.success)
        self.assertIn("RBAC permissions", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_to_be_completed"
    )
    async def test_create_timeout(self, mock_wait):
        mock_wait.side_effect = TimeoutError("timed out")

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(return_value={"metadata": {"resourceVersion": "100"}})
        )

        result = await self.engine.create("test-trigger", podsnapshot_timeout=1)

        self.assertFalse(result.success)
        self.assertIn("timed out", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_to_be_completed"
    )
    async def test_create_watch_failure(self, mock_wait):
        mock_wait.side_effect = RuntimeError("Snapshot failed. Condition: error")

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(return_value={"metadata": {"resourceVersion": "100"}})
        )

        result = await self.engine.create("test-trigger")

        self.assertFalse(result.success)
        self.assertIn("Snapshot creation failed", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_to_be_completed"
    )
    async def test_create_generic_exception(self, mock_wait):
        mock_wait.side_effect = Exception("Something went wrong")

        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(return_value={"metadata": {"resourceVersion": "100"}})
        )

        result = await self.engine.create("test-trigger")

        self.assertFalse(result.success)
        self.assertIn("Server error: Something went wrong", result.error_reason)

    async def test_create_tracks_manual_triggers(self):
        self.mock_k8s_helper.custom_objects_api.create_namespaced_custom_object = (
            AsyncMock(return_value={"metadata": {"resourceVersion": "123"}})
        )

        with patch(
            "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_to_be_completed",
            new_callable=AsyncMock,
        ) as mock_wait:
            mock_result = MagicMock()
            mock_result.snapshot_uid = "uid"
            mock_result.snapshot_timestamp = "ts"
            mock_wait.return_value = mock_result

            await self.engine.create("test-trigger")

        self.assertEqual(len(self.engine.created_manual_triggers), 1)
        self.assertIn("test-trigger", self.engine.created_manual_triggers[0])

    async def test_delete_manual_triggers_success(self):
        self.engine.created_manual_triggers = ["trigger-1", "trigger-2"]
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock()
        )

        await self.engine.delete_manual_triggers()

        self.assertEqual(
            self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object.call_count,
            2,
        )
        self.assertEqual(len(self.engine.created_manual_triggers), 0)

    async def test_delete_manual_triggers_404_ignored(self):
        self.engine.created_manual_triggers = ["trigger-1"]
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock(side_effect=ApiException(status=404))
        )

        await self.engine.delete_manual_triggers()

        self.assertEqual(len(self.engine.created_manual_triggers), 0)

    async def test_delete_manual_triggers_retry(self):
        self.engine.created_manual_triggers = ["trigger-1"]
        call_count = 0

        async def mock_delete(**kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                raise ApiException(status=500)

        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            mock_delete
        )

        with patch(
            "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.asyncio.sleep",
            new_callable=AsyncMock,
        ):
            await self.engine.delete_manual_triggers(max_retries=3)

        self.assertEqual(len(self.engine.created_manual_triggers), 0)

    async def test_list_success(self):
        mock_response = {
            "items": [
                {
                    "metadata": {
                        "name": "snap-1",
                        "creationTimestamp": "2023-01-02T00:00:00Z",
                        "labels": {PODSNAPSHOT_POD_NAME_LABEL: "test-pod"},
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {
                        "name": "snap-2",
                        "creationTimestamp": "2023-01-01T00:00:00Z",
                        "labels": {PODSNAPSHOT_POD_NAME_LABEL: "test-pod"},
                    },
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
            ]
        }
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object = (
            AsyncMock(return_value=mock_response)
        )

        result = await self.engine.list(
            filter_by={"grouping_labels": {"test-label": "test-value"}}
        )

        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 2)
        self.assertEqual(result.snapshots[0].snapshot_uid, "snap-1")
        self.assertEqual(result.snapshots[1].snapshot_uid, "snap-2")

    async def test_list_ready_only_filter(self):
        mock_response = {
            "items": [
                {
                    "metadata": {"name": "ready-snap", "creationTimestamp": "ts"},
                    "status": {"conditions": [{"type": "Ready", "status": "True"}]},
                },
                {
                    "metadata": {"name": "not-ready-snap", "creationTimestamp": "ts2"},
                    "status": {"conditions": [{"type": "Ready", "status": "False"}]},
                },
            ]
        }
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object = (
            AsyncMock(return_value=mock_response)
        )

        result = await self.engine.list(filter_by={"ready_only": False})
        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 2)

    async def test_list_invalid_filter(self):
        result = await self.engine.list(filter_by={"random_key": "random_value"})
        self.assertFalse(result.success)
        self.assertIn("Invalid filter parameters", result.error_reason)

    async def test_list_no_pod_name(self):
        self.get_pod_name_func.return_value = None
        result = await self.engine.list()
        self.assertFalse(result.success)
        self.assertIn("Pod name not found", result.error_reason)

    async def test_list_empty(self):
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object = (
            AsyncMock(return_value={"items": []})
        )
        result = await self.engine.list()
        self.assertTrue(result.success)
        self.assertEqual(len(result.snapshots), 0)

    async def test_list_api_exception(self):
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object = (
            AsyncMock(side_effect=ApiException(500, "Internal Server Error"))
        )
        result = await self.engine.list()
        self.assertFalse(result.success)
        self.assertIn("Failed to list PodSnapshots", result.error_reason)

    async def test_list_generic_exception(self):
        self.mock_k8s_helper.custom_objects_api.list_namespaced_custom_object = (
            AsyncMock(side_effect=ValueError("Unexpected"))
        )
        result = await self.engine.list()
        self.assertFalse(result.success)
        self.assertIn("Unexpected error", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_deletion"
    )
    async def test_delete_success(self, mock_wait):
        mock_wait.return_value = True
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock(return_value={})
        )

        result = await self.engine.delete("target-snap")

        self.assertTrue(result.success)
        self.assertEqual(result.deleted_snapshots, ["target-snap"])

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_deletion"
    )
    async def test_delete_api_exception(self, mock_wait):
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock(side_effect=ApiException(500, "Internal error"))
        )

        result = await self.engine.delete("target-snap")

        self.assertFalse(result.success)
        self.assertIn("Failed to delete PodSnapshot", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_deletion"
    )
    async def test_delete_404_treated_as_gone(self, mock_wait):
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock(side_effect=ApiException(status=404))
        )

        result = await self.engine.delete("target-snap")

        self.assertTrue(result.success)
        self.assertEqual(result.deleted_snapshots, [])
        mock_wait.assert_not_called()

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_deletion"
    )
    async def test_delete_timeout(self, mock_wait):
        mock_wait.return_value = False
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock(return_value={})
        )

        result = await self.engine.delete("target-snap")

        self.assertFalse(result.success)
        self.assertIn("Timed out", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_deletion"
    )
    async def test_delete_all_success(self, mock_wait):
        mock_wait.return_value = True
        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            AsyncMock(return_value={})
        )

        with patch.object(self.engine, "list", new_callable=AsyncMock) as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="s1",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                    SnapshotDetail(
                        snapshot_uid="s2",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                ],
                error_reason="",
                error_code=0,
            )

            result = await self.engine.delete_all()

        self.assertTrue(result.success)
        self.assertEqual(result.deleted_snapshots, ["s1", "s2"])

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_snapshot_engine.async_wait_for_snapshot_deletion"
    )
    async def test_delete_all_partial_failure(self, mock_wait):
        mock_wait.return_value = True

        async def mock_delete(**kwargs):
            if kwargs.get("name") == "s2":
                raise ApiException(500, "error")
            return {}

        self.mock_k8s_helper.custom_objects_api.delete_namespaced_custom_object = (
            mock_delete
        )

        with patch.object(self.engine, "list", new_callable=AsyncMock) as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="s1",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                    SnapshotDetail(
                        snapshot_uid="s2",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                    SnapshotDetail(
                        snapshot_uid="s3",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    ),
                ],
                error_reason="",
                error_code=0,
            )

            result = await self.engine.delete_all()

        self.assertFalse(result.success)
        self.assertEqual(result.deleted_snapshots, ["s1", "s3"])
        self.assertIn("Failed to delete PodSnapshot 's2'", result.error_reason)

    async def test_delete_all_invalid_strategy(self):
        with self.assertRaises(ValueError):
            await self.engine.delete_all(delete_by="invalid")

    async def test_delete_all_by_labels(self):
        with patch.object(
            self.engine, "_execute_deletion", new_callable=AsyncMock
        ) as mock_exec:
            mock_exec.return_value = DeleteSnapshotResult(
                success=True, deleted_snapshots=[], error_reason="", error_code=0
            )
            await self.engine.delete_all(delete_by="labels", label_value={"foo": "bar"})
            mock_exec.assert_called_once_with(labels={"foo": "bar"}, timeout=180)

    async def test_delete_all_by_labels_invalid_value(self):
        with self.assertRaises(ValueError):
            await self.engine.delete_all(delete_by="labels", label_value="not-a-dict")


if __name__ == "__main__":
    unittest.main()
