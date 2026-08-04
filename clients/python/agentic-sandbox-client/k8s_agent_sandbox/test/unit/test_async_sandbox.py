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

"""Unit tests for the async Sandbox connection lifecycle."""

import unittest

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

pytest.importorskip("httpx")
pytest.importorskip("kubernetes_asyncio")

from k8s_agent_sandbox.async_sandbox import AsyncSandbox
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxTracerConfig,
)


class TestAsyncSandbox(unittest.IsolatedAsyncioTestCase):

    @patch("k8s_agent_sandbox.async_sandbox.AsyncFilesystem")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncCommandExecutor")
    @patch("k8s_agent_sandbox.async_sandbox.create_tracer_manager")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncSandboxConnector")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncK8sHelper")
    def setUp(
        self,
        mock_k8s_helper,
        mock_connector,
        mock_create_tracer_manager,
        mock_command_executor,
        mock_filesystem,
    ):
        self.mock_k8s_helper_cls = mock_k8s_helper
        self.mock_connector_cls = mock_connector
        self.mock_create_tracer_manager_func = mock_create_tracer_manager
        self.mock_command_executor_cls = mock_command_executor
        self.mock_filesystem_cls = mock_filesystem

        self.mock_k8s_helper = mock_k8s_helper.return_value
        self.mock_k8s_helper.get_sandbox = AsyncMock()
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_connector = mock_connector.return_value
        self.mock_connector.close = AsyncMock()
        self.mock_tracer_manager = MagicMock()
        self.mock_tracer = MagicMock()
        mock_create_tracer_manager.return_value = (
            self.mock_tracer_manager,
            self.mock_tracer,
        )
        self.mock_command_executor = mock_command_executor.return_value
        self.mock_filesystem = mock_filesystem.return_value

        self.sandbox_id = "test-sandbox"
        self.namespace = "test-namespace"
        self.claim_name = "test-claim"
        self.connection_config = SandboxDirectConnectionConfig(
            api_url="http://test-router:8080",
        )

        self.sandbox = AsyncSandbox(
            claim_name=self.claim_name,
            sandbox_id=self.sandbox_id,
            namespace=self.namespace,
            connection_config=self.connection_config,
        )

    def test_init_with_defaults(self):
        """Tests sandbox initialization with default configurations."""
        self.mock_k8s_helper_cls.assert_called_once()

        self.mock_connector_cls.assert_called_once()
        args, kwargs = self.mock_connector_cls.call_args
        self.assertEqual(kwargs["sandbox_id"], self.sandbox_id)
        self.assertEqual(kwargs["namespace"], self.namespace)
        self.assertEqual(kwargs["connection_config"], self.connection_config)
        self.assertEqual(kwargs["k8s_helper"], self.mock_k8s_helper)

        self.mock_create_tracer_manager_func.assert_called_once()
        self.assertIsInstance(
            self.mock_create_tracer_manager_func.call_args[0][0],
            SandboxTracerConfig,
        )

        self.mock_command_executor_cls.assert_called_once_with(
            self.mock_connector,
            self.mock_tracer,
            "sandbox-client",
        )
        self.mock_filesystem_cls.assert_called_once_with(
            self.mock_connector,
            self.mock_tracer,
            "sandbox-client",
        )

        self.assertEqual(self.sandbox.claim_name, self.claim_name)
        self.assertEqual(self.sandbox.sandbox_id, self.sandbox_id)
        self.assertEqual(self.sandbox.namespace, self.namespace)
        self.assertEqual(self.sandbox.connection_config, self.connection_config)
        self.assertFalse(self.sandbox._is_closed)

    def test_init_requires_connection_config(self):
        """Tests that AsyncSandbox requires an explicit connection configuration."""
        with self.assertRaises(ValueError) as ctx:
            AsyncSandbox(
                claim_name="custom-claim",
                sandbox_id="custom-id",
                namespace="custom-ns",
                connection_config=None,
            )
        self.assertIn("connection_config is required", str(ctx.exception))

    @patch("k8s_agent_sandbox.async_sandbox.AsyncFilesystem")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncCommandExecutor")
    @patch("k8s_agent_sandbox.async_sandbox.create_tracer_manager")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncSandboxConnector")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncK8sHelper")
    def test_init_with_custom_args(
        self,
        mock_k8s_helper,
        mock_connector,
        mock_create_tracer_manager,
        mock_command_executor,
        mock_filesystem,
    ):
        """Tests sandbox initialization with custom arguments."""
        mock_k8s_helper_instance = MagicMock()
        mock_connection_config = MagicMock()
        mock_tracer_config = SandboxTracerConfig(trace_service_name="custom-tracer")
        mock_tracer, mock_manager = MagicMock(), MagicMock()
        mock_create_tracer_manager.return_value = (mock_manager, mock_tracer)

        sandbox = AsyncSandbox(
            sandbox_id="custom-id",
            namespace="custom-ns",
            claim_name="custom-claim",
            connection_config=mock_connection_config,
            tracer_config=mock_tracer_config,
            k8s_helper=mock_k8s_helper_instance,
        )

        mock_k8s_helper.assert_not_called()
        self.assertEqual(sandbox.k8s_helper, mock_k8s_helper_instance)

        mock_connector.assert_called_once_with(
            sandbox_id="custom-id",
            namespace="custom-ns",
            connection_config=mock_connection_config,
            k8s_helper=mock_k8s_helper_instance,
            get_pod_ip=sandbox.get_pod_ip,
        )

        mock_create_tracer_manager.assert_called_once_with(mock_tracer_config)
        mock_command_executor.assert_called_once_with(
            mock_connector.return_value,
            mock_tracer,
            "custom-tracer",
        )
        mock_filesystem.assert_called_once_with(
            mock_connector.return_value,
            mock_tracer,
            "custom-tracer",
        )

    async def test_get_pod_name_with_annotation(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {
                "annotations": {
                    "agents.x-k8s.io/pod-name": "annotated-pod-name",
                },
            },
        }
        self.assertEqual(await self.sandbox.get_pod_name(), "annotated-pod-name")

    async def test_get_pod_name_fallback(self):
        self.mock_k8s_helper.get_sandbox.return_value = None
        self.assertEqual(await self.sandbox.get_pod_name(), self.sandbox_id)

    async def test_get_pod_name_caching(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {
                "annotations": {
                    "agents.x-k8s.io/pod-name": "cached-pod-name",
                },
            },
        }
        self.assertEqual(await self.sandbox.get_pod_name(), "cached-pod-name")

        self.mock_k8s_helper.get_sandbox.reset_mock()
        self.assertEqual(await self.sandbox.get_pod_name(), "cached-pod-name")
        self.mock_k8s_helper.get_sandbox.assert_not_awaited()

    async def test_status_not_found(self):
        self.mock_k8s_helper.get_sandbox.return_value = None
        status, message = await self.sandbox.status()

        self.assertEqual(status, "SandboxNotFound")
        self.assertEqual(message, "Sandbox object not found in Kubernetes.")
        self.mock_k8s_helper.get_sandbox.assert_awaited_once_with(
            self.sandbox_id,
            self.namespace,
        )

    async def test_status_ready(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "conditions": [
                    {"type": "Ready", "status": "True", "message": ""},
                ],
            },
        }
        status, message = await self.sandbox.status()

        self.assertEqual(status, "SandboxReady")
        self.assertEqual(message, "")

    async def test_status_not_ready_with_message(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "conditions": [
                    {
                        "type": "Ready",
                        "status": "False",
                        "message": "Pod is initializing",
                    },
                ],
            },
        }
        status, message = await self.sandbox.status()

        self.assertEqual(status, "SandboxNotReady")
        self.assertEqual(message, "Pod is initializing")

    async def test_status_no_ready_condition(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "conditions": [
                    {"type": "PodScheduled", "status": "True"},
                ],
            },
        }
        status, message = await self.sandbox.status()

        self.assertEqual(status, "SandboxNotReady")
        self.assertEqual(message, "Unknown message")

    async def test_get_pod_ip(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "podIPs": ["10.244.0.5", "fd00::5"],
            },
        }
        self.assertEqual(await self.sandbox.get_pod_ip(), "10.244.0.5")

    async def test_get_pod_ip_returns_none_when_missing(self):
        self.mock_k8s_helper.get_sandbox.return_value = {"status": {}}
        self.assertIsNone(await self.sandbox.get_pod_ip())

    def test_properties(self):
        """Tests the commands and files properties."""
        self.assertEqual(self.sandbox.commands, self.mock_command_executor)
        self.assertEqual(self.sandbox.files, self.mock_filesystem)

    def test_is_active(self):
        """Tests the is_active property."""
        self.assertTrue(self.sandbox.is_active)
        self.sandbox._is_closed = True
        self.assertFalse(self.sandbox.is_active)

    async def test_close_connection(self):
        """Tests the public close_connection method."""
        await self.sandbox.close_connection()

        self.mock_connector.close.assert_awaited_once()
        self.assertIsNone(self.sandbox.commands)
        self.assertIsNone(self.sandbox.files)
        self.mock_tracer_manager.end_lifecycle_span.assert_called_once()
        self.assertTrue(self.sandbox._is_closed)

        self.mock_connector.close.reset_mock()
        await self.sandbox.close_connection()
        self.mock_connector.close.assert_not_awaited()

    @patch("logging.error")
    async def test_close_connection_with_tracing_error(self, mock_logging_error):
        """Tests close_connection with an error in tracing."""
        self.mock_tracer_manager.end_lifecycle_span.side_effect = Exception(
            "Tracer error",
        )
        await self.sandbox.close_connection()

        self.mock_connector.close.assert_awaited_once()
        self.assertTrue(self.sandbox._is_closed)
        mock_logging_error.assert_called_once_with(
            "Failed to end tracing span: Tracer error",
        )

    async def test_terminate(self):
        """Tests the terminate method."""
        with patch.object(
            self.sandbox,
            "close_connection",
            new_callable=AsyncMock,
        ) as mock_close:
            await self.sandbox.terminate()
            mock_close.assert_awaited_once()

        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            self.claim_name,
            self.namespace,
        )
        self.assertIsNone(self.sandbox.claim_name)

    async def test_get_sandbox_name_hash_from_k8s(self):
        """Tests retrieving sandbox name hash from status.selector when it is present."""
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "selector": "agents.x-k8s.io/sandbox-name-hash=abc12345",
            },
        }
        self.assertEqual(await self.sandbox.get_sandbox_name_hash(), "abc12345")
        self.mock_k8s_helper.get_sandbox.assert_awaited_once_with(
            self.sandbox_id,
            self.namespace,
        )

    async def test_get_sandbox_name_hash_returns_none_when_selector_missing(self):
        """Tests that get_sandbox_name_hash returns None when status.selector is missing."""
        self.mock_k8s_helper.get_sandbox.return_value = {"status": {}}
        self.assertIsNone(await self.sandbox.get_sandbox_name_hash())
        self.mock_k8s_helper.get_sandbox.assert_awaited_once_with(
            self.sandbox_id,
            self.namespace,
        )

    async def test_get_sandbox_name_hash_caching(self):
        """Tests that sandbox name hash is cached and does not query Kubernetes repeatedly."""
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "selector": "agents.x-k8s.io/sandbox-name-hash=mycachedhash",
            },
        }
        self.assertEqual(await self.sandbox.get_sandbox_name_hash(), "mycachedhash")

        self.mock_k8s_helper.get_sandbox.reset_mock()
        self.assertEqual(await self.sandbox.get_sandbox_name_hash(), "mycachedhash")
        self.mock_k8s_helper.get_sandbox.assert_not_awaited()


class TestAsyncSandboxTerminateIdempotent(unittest.IsolatedAsyncioTestCase):
    """`AsyncSandbox.terminate()` must be idempotent."""

    @patch("k8s_agent_sandbox.async_sandbox.AsyncFilesystem")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncCommandExecutor")
    @patch("k8s_agent_sandbox.async_sandbox.create_tracer_manager")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncSandboxConnector")
    def _build_sandbox(self, mock_connector, mock_tracer, mock_cmd, mock_files):
        mock_connector.return_value.close = AsyncMock()
        mock_tracer.return_value = (MagicMock(), MagicMock())
        k8s_helper = MagicMock()
        k8s_helper.delete_sandbox_claim = AsyncMock()
        k8s_helper.get_sandbox = AsyncMock()
        return (
            AsyncSandbox(
                claim_name="my-claim",
                sandbox_id="my-claim",
                namespace="demo",
                connection_config=SandboxDirectConnectionConfig(
                    api_url="http://test-router:8080",
                ),
                tracer_config=SandboxTracerConfig(),
                k8s_helper=k8s_helper,
            ),
            k8s_helper,
        )

    async def test_second_terminate_does_not_redelete(self):
        sandbox, helper = self._build_sandbox()

        await sandbox.terminate()
        self.assertEqual(helper.delete_sandbox_claim.await_count, 1)
        self.assertIsNone(sandbox.claim_name)

        await sandbox.terminate()
        self.assertEqual(helper.delete_sandbox_claim.await_count, 1)

    async def test_failed_terminate_preserves_claim_name_for_retry(self):
        sandbox, helper = self._build_sandbox()

        helper.delete_sandbox_claim.side_effect = RuntimeError("transient 500")

        with self.assertRaisesRegex(RuntimeError, "transient 500"):
            await sandbox.terminate()

        self.assertEqual(sandbox.claim_name, "my-claim")
        self.assertEqual(helper.delete_sandbox_claim.await_count, 1)

        helper.delete_sandbox_claim.side_effect = None
        await sandbox.terminate()
        self.assertEqual(helper.delete_sandbox_claim.await_count, 2)
        self.assertIsNone(sandbox.claim_name)


if __name__ == "__main__":
    unittest.main()
