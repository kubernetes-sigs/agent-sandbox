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

import asyncio
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

pytest.importorskip("kubernetes_asyncio")
pytest.importorskip("httpx")

from kubernetes_asyncio.client import ApiException

from k8s_agent_sandbox.gke_extensions.snapshots.async_podsnapshot_client import (
    AsyncPodSnapshotSandboxClient,
)
from k8s_agent_sandbox.gke_extensions.snapshots.async_sandbox_with_snapshot_support import (
    AsyncSandboxWithSnapshotSupport,
)
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
)
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig


class TestAsyncPodSnapshotSandboxClient(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

        self.config = SandboxDirectConnectionConfig(
            api_url="http://test-router:8080", server_port=8888
        )

    def _make_client(self):
        return AsyncPodSnapshotSandboxClient(connection_config=self.config)

    async def test_sandbox_class_is_async_snapshot_support(self):
        client = self._make_client()
        self.assertEqual(client.sandbox_class, AsyncSandboxWithSnapshotSupport)

    async def test_ensure_snapshot_ready_crd_installed(self):
        client = self._make_client()
        mock_k8s = client.k8s_helper
        mock_k8s._ensure_initialized = AsyncMock()

        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list = MagicMock()
        mock_resource_list.resources = [mock_resource]
        mock_k8s.custom_objects_api.get_api_resources = AsyncMock(
            return_value=mock_resource_list
        )

        await client._ensure_snapshot_ready()

        self.assertTrue(client._snapshot_crd_verified)
        mock_k8s.custom_objects_api.get_api_resources.assert_called_once_with(
            group=PODSNAPSHOT_API_GROUP, version=PODSNAPSHOT_API_VERSION
        )

    async def test_ensure_snapshot_ready_crd_not_installed(self):
        client = self._make_client()
        mock_k8s = client.k8s_helper
        mock_k8s._ensure_initialized = AsyncMock()
        mock_k8s.custom_objects_api.get_api_resources = AsyncMock(return_value=None)

        with self.assertRaises(RuntimeError) as ctx:
            await client._ensure_snapshot_ready()

        self.assertIn("Pod Snapshot Controller is not ready", str(ctx.exception))
        self.assertFalse(client._snapshot_crd_verified)

    async def test_ensure_snapshot_ready_api_exception_403(self):
        client = self._make_client()
        mock_k8s = client.k8s_helper
        mock_k8s._ensure_initialized = AsyncMock()
        mock_k8s.custom_objects_api.get_api_resources = AsyncMock(
            side_effect=ApiException(status=403)
        )

        with self.assertRaises(RuntimeError) as ctx:
            await client._ensure_snapshot_ready()
        self.assertIn("Pod Snapshot Controller is not ready", str(ctx.exception))

    async def test_ensure_snapshot_ready_api_exception_500_reraises(self):
        client = self._make_client()
        mock_k8s = client.k8s_helper
        mock_k8s._ensure_initialized = AsyncMock()
        mock_k8s.custom_objects_api.get_api_resources = AsyncMock(
            side_effect=ApiException(status=500)
        )

        with self.assertRaises(ApiException):
            await client._ensure_snapshot_ready()

    async def test_ensure_snapshot_ready_called_once_under_concurrency(self):
        """Two concurrent calls should result in only one CRD check."""
        client = self._make_client()
        mock_k8s = client.k8s_helper
        mock_k8s._ensure_initialized = AsyncMock()

        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list = MagicMock()
        mock_resource_list.resources = [mock_resource]
        mock_k8s.custom_objects_api.get_api_resources = AsyncMock(
            return_value=mock_resource_list
        )

        await asyncio.gather(
            client._ensure_snapshot_ready(),
            client._ensure_snapshot_ready(),
        )

        mock_k8s.custom_objects_api.get_api_resources.assert_called_once()

    async def test_aenter_calls_ensure_ready(self):
        client = self._make_client()
        mock_k8s = client.k8s_helper
        mock_k8s._ensure_initialized = AsyncMock()
        mock_k8s.close = AsyncMock()

        mock_resource = MagicMock()
        mock_resource.kind = PODSNAPSHOT_API_KIND
        mock_resource_list = MagicMock()
        mock_resource_list.resources = [mock_resource]
        mock_k8s.custom_objects_api.get_api_resources = AsyncMock(
            return_value=mock_resource_list
        )

        async with client as c:
            self.assertTrue(c._snapshot_crd_verified)

    async def test_create_sandbox_calls_ensure_ready(self):
        client = self._make_client()

        with (
            patch.object(
                client, "_ensure_snapshot_ready", new_callable=AsyncMock
            ) as mock_ensure,
            patch(
                "k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.create_sandbox",
                new_callable=AsyncMock,
            ) as mock_super,
        ):
            mock_super.return_value = MagicMock(spec=AsyncSandboxWithSnapshotSupport)

            await client.create_sandbox("test-template", "test-ns")

            mock_ensure.assert_called_once()
            mock_super.assert_called_once()

    async def test_get_sandbox_calls_ensure_ready(self):
        client = self._make_client()

        with (
            patch.object(
                client, "_ensure_snapshot_ready", new_callable=AsyncMock
            ) as mock_ensure,
            patch(
                "k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.get_sandbox",
                new_callable=AsyncMock,
            ) as mock_super,
        ):
            mock_super.return_value = MagicMock(spec=AsyncSandboxWithSnapshotSupport)

            await client.get_sandbox("test-claim", "test-ns")

            mock_ensure.assert_called_once()
            mock_super.assert_called_once()


if __name__ == "__main__":
    unittest.main()
