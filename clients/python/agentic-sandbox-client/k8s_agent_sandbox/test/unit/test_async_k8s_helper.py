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

from k8s_agent_sandbox.async_k8s_helper import AsyncK8sHelper
from k8s_agent_sandbox.exceptions import SandboxMetadataError, SandboxTemplateNotFoundError


class TestAsyncK8sHelperCreateSandboxClaim(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.custom_objects_api.create_namespaced_custom_object = AsyncMock()
        self.helper.core_v1_api = MagicMock()

    async def test_lifecycle_included_in_manifest(self):
        lifecycle = {
            "shutdownTime": "2026-12-31T23:59:59Z",
            "shutdownPolicy": "Delete",
        }
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", lifecycle=lifecycle
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["spec"]["warmPoolRef"]["name"], "test-warmpool")

    async def test_no_lifecycle_omits_key(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace"
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertNotIn("lifecycle", body["spec"])

    async def test_lifecycle_with_labels_and_annotations(self):
        lifecycle = {
            "shutdownTime": "2026-06-15T12:00:00Z",
            "shutdownPolicy": "Delete",
        }
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"key": "val"},
            labels={"agent": "test"},
            lifecycle=lifecycle,
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["metadata"]["labels"], {"agent": "test"})
        self.assertEqual(body["metadata"]["annotations"], {"key": "val"})


class TestAsyncK8sHelperResolveSandboxName(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.core_v1_api = MagicMock()

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_resolve_sandbox_name_template_not_found(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [
                        {
                            "type": "Ready",
                            "status": "False",
                            "reason": "TemplateNotFound",
                            "message": "Template 'non-existent-template' not found"
                        }
                    ]
                }
            }
        }

        async def mock_stream(*args, **kwargs):
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        with self.assertRaises(SandboxTemplateNotFoundError) as context:
            await self.helper.resolve_sandbox_name("test-claim", "default", timeout=5)

        self.assertIn("Template 'non-existent-template' not found", str(context.exception))

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_resolve_sandbox_name_deleted_event(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        mock_event = {
            "type": "DELETED",
            "object": {
                "metadata": {"name": "test-claim"}
            }
        }

        async def mock_stream(*args, **kwargs):
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        with self.assertRaises(SandboxMetadataError) as context:
            await self.helper.resolve_sandbox_name("test-claim", "default", timeout=5)

        self.assertIn("SandboxClaim 'test-claim' was deleted while resolving sandbox name", str(context.exception))


class TestAsyncK8sHelperWaitForSandboxReady(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()

    async def test_returns_first_pod_ip_when_ready(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "status": {
                        "conditions": [{"type": "Ready", "status": "True"}],
                        "podIPs": ["10.244.0.5", "fd00::5"],
                    }
                },
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            result = await self.helper.wait_for_sandbox_ready("my-sandbox", "default", timeout=10)

        self.assertEqual(result, "10.244.0.5")

    async def test_returns_none_when_no_pod_ips(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "status": {
                        "conditions": [{"type": "Ready", "status": "True"}],
                    }
                },
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            result = await self.helper.wait_for_sandbox_ready("my-sandbox", "default", timeout=10)

        self.assertIsNone(result)

class TestAsyncK8sHelperDeleteSandboxClaim(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.core_v1_api = MagicMock()

    async def test_delete_404_is_ignored(self):
        from kubernetes_asyncio import client as async_client
        exc = async_client.ApiException(status=404)
        self.helper.custom_objects_api.delete_namespaced_custom_object = AsyncMock(side_effect=exc)

        await self.helper.delete_sandbox_claim("missing-claim", "default")

    async def test_delete_non_404_reraises(self):
        from kubernetes_asyncio import client as async_client
        exc = async_client.ApiException(status=403)
        self.helper.custom_objects_api.delete_namespaced_custom_object = AsyncMock(side_effect=exc)

        with self.assertRaises(async_client.ApiException) as ctx:
            await self.helper.delete_sandbox_claim("claim", "default")
        self.assertEqual(ctx.exception.status, 403)


if __name__ == "__main__":
    unittest.main()
