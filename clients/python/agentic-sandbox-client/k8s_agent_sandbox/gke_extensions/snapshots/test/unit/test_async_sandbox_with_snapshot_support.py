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
pytest.importorskip("httpx")

from kubernetes_asyncio.client import ApiException

from k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support import (
    AsyncSandboxWithSnapshotSupport,
)
from k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine import (
    ListSnapshotResult,
    SnapshotDetail,
    SnapshotResponse,
)
from k8s_agent_sandbox.constants import (
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
    POD_NAME_ANNOTATION,
)
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig


def _make_sandbox(mock_k8s_helper=None):
    """Helper to construct an AsyncSandboxWithSnapshotSupport with mocked deps."""
    config = SandboxDirectConnectionConfig(
        api_url="http://test-router:8080", server_port=8888
    )
    k8s = mock_k8s_helper or MagicMock()
    k8s._ensure_initialized = AsyncMock()
    k8s.custom_objects_api = MagicMock()
    k8s.core_v1_api = MagicMock()

    with (
        patch("k8s_agent_sandbox.async_sandbox.AsyncSandboxConnector"),
        patch(
            "k8s_agent_sandbox.async_sandbox.create_tracer_manager",
            return_value=(None, None),
        ),
        patch("k8s_agent_sandbox.async_sandbox.AsyncCommandExecutor"),
        patch("k8s_agent_sandbox.async_sandbox.AsyncFilesystem"),
    ):
        sandbox = AsyncSandboxWithSnapshotSupport(
            claim_name="test-claim",
            sandbox_id="test-id",
            namespace="test-ns",
            connection_config=config,
            k8s_helper=k8s,
        )
    return sandbox


class TestAsyncSandboxWithSnapshotSupport(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.mock_k8s = MagicMock()
        self.mock_k8s._ensure_initialized = AsyncMock()
        self.mock_k8s.custom_objects_api = MagicMock()
        self.mock_k8s.core_v1_api = MagicMock()
        self.sandbox = _make_sandbox(self.mock_k8s)

    async def test_snapshots_property(self):
        self.assertIsNotNone(self.sandbox.snapshots)

    async def test_is_active(self):
        self.assertTrue(self.sandbox.is_active)

    async def test_resolve_pod_name_bypasses_cache(self):
        self.mock_k8s.get_sandbox = AsyncMock(
            return_value={"metadata": {"annotations": {POD_NAME_ANNOTATION: "new-pod"}}}
        )
        self.sandbox._pod_name = "old-pod"

        result = await self.sandbox._resolve_pod_name()
        self.assertEqual(result, "new-pod")
        # Ensure it didn't mutate the cache
        self.assertEqual(self.sandbox._pod_name, "old-pod")

    # --- is_restored_from_snapshot ---

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_check_pod_restored_from_snapshot"
    )
    async def test_is_restored_from_snapshot_success(self, mock_check):
        from k8s_agent_sandbox.gke_extensions.snapshots.async_utils import (
            RestoreCheckResult,
        )

        mock_check.return_value = RestoreCheckResult(
            success=True, error_reason="", error_code=0
        )
        self.sandbox.get_pod_name = AsyncMock(return_value="test-pod")

        result = await self.sandbox.is_restored_from_snapshot("test-uid")

        self.assertTrue(result.success)
        mock_check.assert_called_once_with(
            k8s_helper=self.mock_k8s,
            namespace="test-ns",
            pod_name="test-pod",
            snapshot_uid="test-uid",
        )

    async def test_is_restored_from_snapshot_empty_uid(self):
        result = await self.sandbox.is_restored_from_snapshot("")
        self.assertFalse(result.success)
        self.assertIn("Snapshot UID cannot be empty", result.error_reason)

    async def test_is_restored_from_snapshot_no_pod_name(self):
        self.sandbox.get_pod_name = AsyncMock(return_value=None)
        result = await self.sandbox.is_restored_from_snapshot("test-uid")
        self.assertFalse(result.success)
        self.assertIn("Pod name not found", result.error_reason)

    # --- is_suspended ---

    async def test_is_suspended_true(self):
        self.mock_k8s.custom_objects_api.get_namespaced_custom_object = AsyncMock(
            return_value={"spec": {"replicas": 0}, "status": {}}
        )
        self.assertTrue(await self.sandbox.is_suspended())

    async def test_is_suspended_false(self):
        self.mock_k8s.custom_objects_api.get_namespaced_custom_object = AsyncMock(
            return_value={"spec": {"replicas": 1}, "status": {"podIPs": ["10.0.0.1"]}}
        )
        self.assertFalse(await self.sandbox.is_suspended())

    async def test_is_suspended_exception_returns_false(self):
        self.mock_k8s.custom_objects_api.get_namespaced_custom_object = AsyncMock(
            side_effect=Exception("API error")
        )
        self.assertFalse(await self.sandbox.is_suspended())

    # --- suspend ---

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_termination"
    )
    async def test_suspend_success_with_snapshot(self, mock_wait):
        mock_wait.return_value = True
        self.sandbox.get_pod_name = AsyncMock(return_value="test-pod")
        self.mock_k8s.core_v1_api.read_namespaced_pod = AsyncMock(
            return_value=MagicMock(metadata=MagicMock(uid="pod-uid"))
        )
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=False
            ),
            patch.object(
                self.sandbox.snapshots, "create", new_callable=AsyncMock
            ) as mock_create,
        ):
            mock_create.return_value = SnapshotResponse(
                success=True,
                trigger_name="t",
                snapshot_uid="uid-123",
                snapshot_timestamp="ts",
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox.suspend()

        self.assertTrue(result.success)
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object.assert_called_once_with(
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            namespace="test-ns",
            plural=SANDBOX_PLURAL_NAME,
            name="test-id",
            body={"spec": {"replicas": 0}},
        )

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_termination"
    )
    async def test_suspend_without_snapshot(self, mock_wait):
        mock_wait.return_value = True
        self.sandbox.get_pod_name = AsyncMock(return_value="test-pod")
        self.mock_k8s.core_v1_api.read_namespaced_pod = AsyncMock(
            return_value=MagicMock(metadata=MagicMock(uid="pod-uid"))
        )
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()

        with patch.object(
            self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=False
        ):
            result = await self.sandbox.suspend(snapshot_before_suspend=False)

        self.assertTrue(result.success)
        self.assertIsNone(result.snapshot_response)

    async def test_suspend_already_suspended(self):
        with patch.object(
            self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
        ):
            result = await self.sandbox.suspend()

        self.assertTrue(result.success)

    async def test_suspend_snapshot_fails_no_scaledown(self):
        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=False
            ),
            patch.object(
                self.sandbox.snapshots, "create", new_callable=AsyncMock
            ) as mock_create,
        ):
            mock_create.return_value = SnapshotResponse(
                success=False,
                trigger_name="t",
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason="Pod not found",
                error_code=1,
            )

            result = await self.sandbox.suspend()

        self.assertFalse(result.success)
        self.assertIn("Pod not found", result.error_reason)
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object.assert_not_called()

    async def test_suspend_scale_down_fails(self):
        self.sandbox.get_pod_name = AsyncMock(return_value="test-pod")
        self.mock_k8s.core_v1_api.read_namespaced_pod = AsyncMock(
            return_value=MagicMock(metadata=MagicMock(uid="pod-uid"))
        )
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock(
            side_effect=ApiException("Failed")
        )

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=False
            ),
            patch.object(
                self.sandbox.snapshots, "create", new_callable=AsyncMock
            ) as mock_create,
        ):
            mock_create.return_value = SnapshotResponse(
                success=True,
                trigger_name="t",
                snapshot_uid="uid",
                snapshot_timestamp="ts",
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox.suspend()

        self.assertFalse(result.success)
        self.assertIn("Failed", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_termination"
    )
    async def test_suspend_timeout(self, mock_wait):
        mock_wait.return_value = False
        self.sandbox.get_pod_name = AsyncMock(return_value="test-pod")
        self.mock_k8s.core_v1_api.read_namespaced_pod = AsyncMock(
            return_value=MagicMock(metadata=MagicMock(uid="pod-uid"))
        )
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=False
            ),
            patch.object(
                self.sandbox.snapshots, "create", new_callable=AsyncMock
            ) as mock_create,
        ):
            mock_create.return_value = SnapshotResponse(
                success=True,
                trigger_name="t",
                snapshot_uid="uid",
                snapshot_timestamp="ts",
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox.suspend(wait_timeout=1)

        self.assertFalse(result.success)
        self.assertIn("Timed out", result.error_reason)

    # --- resume ---

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_ready"
    )
    async def test_resume_no_snapshots(self, mock_wait):
        mock_wait.return_value = True
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox.snapshots, "list", new_callable=AsyncMock
            ) as mock_list,
        ):
            mock_list.return_value = ListSnapshotResult(
                success=True, snapshots=[], error_reason="", error_code=0
            )

            result = await self.sandbox.resume()

        self.assertTrue(result.success)
        self.assertFalse(result.restored_from_snapshot)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_check_pod_restored_from_snapshot"
    )
    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_ready"
    )
    async def test_resume_restored_from_snapshot(self, mock_wait, mock_check):
        from k8s_agent_sandbox.gke_extensions.snapshots.async_utils import (
            RestoreCheckResult,
        )

        mock_wait.return_value = True
        mock_check.return_value = RestoreCheckResult(
            success=True, error_reason="", error_code=0
        )
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()
        self.mock_k8s.get_sandbox = AsyncMock(
            return_value={"metadata": {"annotations": {POD_NAME_ANNOTATION: "new-pod"}}}
        )

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox.snapshots, "list", new_callable=AsyncMock
            ) as mock_list,
        ):
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="uid-123",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    )
                ],
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox.resume()

        self.assertTrue(result.success)
        self.assertTrue(result.restored_from_snapshot)
        self.assertEqual(result.snapshot_uid, "uid-123")

    async def test_resume_already_running(self):
        with patch.object(
            self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=False
        ):
            result = await self.sandbox.resume()

        self.assertTrue(result.success)
        self.assertFalse(result.restored_from_snapshot)

    async def test_resume_get_snapshot_uid_failure(self):
        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox,
                "_get_latest_snapshot_uid",
                new_callable=AsyncMock,
                side_effect=RuntimeError("List error"),
            ),
        ):
            result = await self.sandbox.resume()

        self.assertFalse(result.success)
        self.assertIn(
            "Failed to get latest snapshot UID: List error", result.error_reason
        )

    async def test_resume_scale_up_fails(self):
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock(
            side_effect=ApiException("Failed")
        )

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox,
                "_get_latest_snapshot_uid",
                new_callable=AsyncMock,
                return_value="uid-123",
            ),
        ):
            result = await self.sandbox.resume()

        self.assertFalse(result.success)
        self.assertIn("Failed", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_ready"
    )
    async def test_resume_timeout(self, mock_wait):
        mock_wait.return_value = False
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox.snapshots, "list", new_callable=AsyncMock
            ) as mock_list,
        ):
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="uid-123",
                        source_pod="p",
                        creation_timestamp="ts",
                        status="Ready",
                    )
                ],
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox.resume(wait_timeout=1)

        self.assertFalse(result.success)
        self.assertIn("Timed out", result.error_reason)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_ready"
    )
    async def test_resume_invalidates_pod_name_cache(self, mock_wait):
        """Resume must clear _pod_name so subsequent calls re-fetch the new pod."""
        mock_wait.return_value = True
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()
        self.sandbox._pod_name = "old-pod"

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox.snapshots, "list", new_callable=AsyncMock
            ) as mock_list,
        ):
            mock_list.return_value = ListSnapshotResult(
                success=True, snapshots=[], error_reason="", error_code=0
            )

            await self.sandbox.resume()

        self.assertIsNone(self.sandbox._pod_name)

    @patch(
        "k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support.async_wait_for_pod_ready"
    )
    async def test_resume_uses_resolve_pod_name_for_wait(self, mock_wait):
        """Resume must pass _resolve_pod_name (not get_pod_name) to the wait helper."""
        mock_wait.return_value = True
        self.mock_k8s.custom_objects_api.patch_namespaced_custom_object = AsyncMock()

        with (
            patch.object(
                self.sandbox, "is_suspended", new_callable=AsyncMock, return_value=True
            ),
            patch.object(
                self.sandbox.snapshots, "list", new_callable=AsyncMock
            ) as mock_list,
        ):
            mock_list.return_value = ListSnapshotResult(
                success=True, snapshots=[], error_reason="", error_code=0
            )

            await self.sandbox.resume()

        # Verify _resolve_pod_name was passed as the callback (bound method)
        call_args = mock_wait.call_args
        get_pod_func = call_args[0][2]  # 3rd positional arg
        self.assertEqual(get_pod_func, self.sandbox._resolve_pod_name)

    # --- terminate ---

    async def test_terminate(self):
        with patch.object(
            self.sandbox.snapshots, "delete_manual_triggers", new_callable=AsyncMock
        ) as mock_cleanup:
            self.sandbox.connector = MagicMock()
            self.sandbox.connector.close = AsyncMock()
            self.mock_k8s.delete_sandbox_claim = AsyncMock()

            await self.sandbox.terminate()

            mock_cleanup.assert_called_once()
            self.assertIsNone(self.sandbox._snapshots)

    async def test_terminate_cleanup_fails_still_terminates(self):
        self.sandbox._snapshots.delete_manual_triggers = AsyncMock(
            side_effect=Exception("cleanup error")
        )
        self.sandbox.connector = MagicMock()
        self.sandbox.connector.close = AsyncMock()
        self.mock_k8s.delete_sandbox_claim = AsyncMock()

        await self.sandbox.terminate()

        self.mock_k8s.delete_sandbox_claim.assert_called_once()
        self.assertIsNone(self.sandbox._snapshots)

    # --- _get_latest_snapshot_uid ---

    async def test_get_latest_snapshot_uid_success(self):
        with patch.object(
            self.sandbox.snapshots, "list", new_callable=AsyncMock
        ) as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[
                    SnapshotDetail(
                        snapshot_uid="uid-1",
                        source_pod="p",
                        creation_timestamp="ts2",
                        status="Ready",
                    ),
                    SnapshotDetail(
                        snapshot_uid="uid-2",
                        source_pod="p",
                        creation_timestamp="ts1",
                        status="Ready",
                    ),
                ],
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox._get_latest_snapshot_uid()

        self.assertEqual(result, "uid-1")

    async def test_get_latest_snapshot_uid_empty(self):
        with patch.object(
            self.sandbox.snapshots, "list", new_callable=AsyncMock
        ) as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=True,
                snapshots=[],
                error_reason="",
                error_code=0,
            )

            result = await self.sandbox._get_latest_snapshot_uid()

        self.assertIsNone(result)

    async def test_get_latest_snapshot_uid_list_failure(self):
        with patch.object(
            self.sandbox.snapshots, "list", new_callable=AsyncMock
        ) as mock_list:
            mock_list.return_value = ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason="List failed",
                error_code=1,
            )

            with self.assertRaises(RuntimeError) as ctx:
                await self.sandbox._get_latest_snapshot_uid()

            self.assertIn(
                "Snapshot list request failed: List failed", str(ctx.exception)
            )


if __name__ == "__main__":
    unittest.main()
