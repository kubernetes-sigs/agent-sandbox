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
            "test-claim", "test-template", "test-namespace", lifecycle=lifecycle
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["spec"]["sandboxTemplateRef"]["name"], "test-template")

    async def test_no_lifecycle_omits_key(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace"
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
            "test-claim", "test-template", "test-namespace",
            annotations={"key": "val"},
            labels={"agent": "test"},
            lifecycle=lifecycle,
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["metadata"]["labels"], {"agent": "test"})
        self.assertEqual(body["metadata"]["annotations"], {"key": "val"})

    async def test_create_claim_with_warmpool_none(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace", warmpool="none"
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["warmpool"], "none")

    async def test_create_claim_with_specific_warmpool(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace", warmpool="custom-pool"
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["warmpool"], "custom-pool")

    async def test_create_claim_warmpool_omitted(self):
        await self.helper.create_sandbox_claim("test-claim", "test-template", "test-namespace")

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertNotIn("warmpool", body["spec"])


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


@patch("k8s_agent_sandbox.async_k8s_helper.normalize_kubernetes_auth_config")
@patch("k8s_agent_sandbox.async_k8s_helper.client")
@patch("k8s_agent_sandbox.async_k8s_helper.config")
class TestAsyncK8sHelperNormalization(unittest.IsolatedAsyncioTestCase):

    async def test_async_k8s_helper_init_calls_normalization(self, mock_config, mock_client, mock_normalize):
        """Test that AsyncK8sHelper._ensure_initialized calls normalize_kubernetes_auth_config."""
        # Setup mocks
        mock_config.ConfigException = Exception
        mock_config.load_incluster_config.side_effect = Exception()
        mock_config.load_kube_config = AsyncMock()
        mock_api_client = MagicMock()
        mock_client.ApiClient.return_value = mock_api_client

        helper = AsyncK8sHelper()
        await helper._ensure_initialized()

        # Verify normalization was called with an explicit configuration instance
        mock_normalize.assert_called_once()
        self.assertIn('configuration', mock_normalize.call_args.kwargs)
        # Verify the return value of normalize was passed to ApiClient
        mock_client.ApiClient.assert_called_once_with(configuration=mock_normalize.return_value)

class TestAsyncK8sHelperClose(unittest.IsolatedAsyncioTestCase):

    async def test_close_nulls_state(self):
        """Test that close() calls ApiClient.close() and nulls all API attributes."""
        helper = AsyncK8sHelper()
        mock_api_client = AsyncMock()
        helper._api_client = mock_api_client
        helper.custom_objects_api = MagicMock()
        helper.core_v1_api = MagicMock()
        helper._initialized = True

        await helper.close()

        mock_api_client.close.assert_awaited_once()
        self.assertIsNone(helper._api_client)
        self.assertIsNone(helper.custom_objects_api)
        self.assertIsNone(helper.core_v1_api)
        self.assertFalse(helper._initialized)

    async def test_close_is_idempotent(self):
        """Test that calling close() twice only closes the ApiClient once."""
        helper = AsyncK8sHelper()
        mock_api_client = AsyncMock()
        helper._api_client = mock_api_client
        helper._initialized = True

        await helper.close()
        await helper.close()

        mock_api_client.close.assert_awaited_once()
        self.assertIsNone(helper._api_client)


if __name__ == "__main__":
    unittest.main()
