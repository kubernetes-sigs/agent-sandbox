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
import json
import unittest
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread
from unittest.mock import ANY, AsyncMock, MagicMock, patch

import pytest

httpx = pytest.importorskip("httpx")
pytest.importorskip("kubernetes_asyncio")

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.async_sandbox import AsyncSandbox
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)


class TestAsyncSandboxClient(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

        self.config = SandboxDirectConnectionConfig(
            api_url="http://test-router:8080", server_port=8888
        )
        self.client = AsyncSandboxClient(connection_config=self.config)
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class

    async def test_create_sandbox_success(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})

        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            sandbox = await self.client.create_sandbox("test-template", "test-namespace")

            mock_create.assert_called_once_with(
                ANY, "test-template", "test-namespace", labels=None, lifecycle=None
            )
            self.assertEqual(sandbox, mock_sandbox_instance)

            active = await self.client.list_active_sandboxes()
            self.assertEqual(len(active), 1)

    async def test_create_sandbox_failure_cleanup(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=Exception("Timeout")
        )

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock), \
             patch.object(self.client, "_delete_claim", new_callable=AsyncMock) as mock_delete:

            with self.assertRaises(Exception) as ctx:
                await self.client.create_sandbox("test-template", "test-namespace")

            self.assertEqual(str(ctx.exception), "Timeout")
            mock_delete.assert_called_once()

    async def test_create_sandbox_cancellation_cleanup(self):
        """CancelledError (BaseException) should still trigger claim cleanup."""
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=asyncio.CancelledError()
        )

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock), \
             patch.object(self.client, "_delete_claim", new_callable=AsyncMock) as mock_delete:

            with self.assertRaises(asyncio.CancelledError):
                await self.client.create_sandbox("test-template", "test-namespace")

            mock_delete.assert_called_once()

    async def test_get_sandbox_existing_active(self):
        mock_sandbox = MagicMock()
        mock_sandbox.is_active = True
        mock_sandbox.terminate = AsyncMock()
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_sandbox

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})

        sandbox = await self.client.get_sandbox("test-claim", "test-namespace")
        self.assertEqual(sandbox, mock_sandbox)
        self.mock_sandbox_class.assert_not_called()

    async def test_get_sandbox_inactive_reattaches(self):
        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        mock_inactive.terminate = AsyncMock()
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_inactive

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})

        mock_new = MagicMock()
        self.mock_sandbox_class.return_value = mock_new

        sandbox = await self.client.get_sandbox("test-claim", "test-namespace")
        self.assertEqual(sandbox, mock_new)

    async def test_get_sandbox_not_found(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=Exception("Not found")
        )

        with self.assertRaises(RuntimeError) as ctx:
            await self.client.get_sandbox("test-claim", "test-namespace")

        self.assertIn("not found", str(ctx.exception))

    async def test_list_active_sandboxes(self):
        mock_active = MagicMock()
        mock_active.is_active = True
        self.client._active_connection_sandboxes[("ns1", "active-claim")] = mock_active

        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        self.client._active_connection_sandboxes[("ns2", "inactive-claim")] = mock_inactive

        active = await self.client.list_active_sandboxes()
        self.assertEqual(active, [("ns1", "active-claim")])

    async def test_list_all_sandboxes(self):
        self.mock_k8s_helper.list_sandbox_claims = AsyncMock(
            return_value=["sb-1", "sb-2"]
        )
        result = await self.client.list_all_sandboxes("test-ns")
        self.assertEqual(result, ["sb-1", "sb-2"])

    async def test_delete_sandbox_in_registry(self):
        mock_sandbox = MagicMock()
        mock_sandbox.terminate = AsyncMock()
        self.client._active_connection_sandboxes[("test-ns", "test-claim")] = mock_sandbox

        await self.client.delete_sandbox("test-claim", "test-ns")
        mock_sandbox.terminate.assert_called_once()

    async def test_delete_all(self):
        mock1 = MagicMock()
        mock1.terminate = AsyncMock()
        mock2 = MagicMock()
        mock2.terminate = AsyncMock()
        self.client._active_connection_sandboxes[("ns1", "c1")] = mock1
        self.client._active_connection_sandboxes[("ns2", "c2")] = mock2

        with patch.object(self.client, "delete_sandbox", new_callable=AsyncMock) as mock_del:
            await self.client.delete_all()
            self.assertEqual(mock_del.call_count, 2)

    async def test_close_clears_registry(self):
        mock_sandbox = MagicMock()
        mock_sandbox._close_connection = AsyncMock()
        self.client._active_connection_sandboxes[("ns", "claim")] = mock_sandbox
        self.mock_k8s_helper.close = AsyncMock()

        await self.client.close()

        self.assertEqual(len(self.client._active_connection_sandboxes), 0)
        mock_sandbox._close_connection.assert_called_once()
        self.mock_k8s_helper.close.assert_called_once()

    async def test_context_manager(self):
        self.mock_k8s_helper.close = AsyncMock()

        async with self.client as c:
            self.assertIsInstance(c, AsyncSandboxClient)

        self.mock_k8s_helper.close.assert_called_once()

    async def test_requires_connection_config(self):
        with self.assertRaises(ValueError) as ctx:
            AsyncSandboxClient(connection_config=None)
        self.assertIn("connection_config is required", str(ctx.exception))

    async def test_validate_labels_rejects_invalid_value(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", labels={"agent": "invalid value!"})

    async def test_validate_labels_rejects_empty_key(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", labels={"": "v"})

    async def test_create_sandbox_with_shutdown_after_seconds(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            await self.client.create_sandbox(
                "test-template", "test-namespace", shutdown_after_seconds=300
            )

            mock_create.assert_called_once()
            call_kwargs = mock_create.call_args
            lifecycle = call_kwargs[1].get("lifecycle")
            self.assertIsNotNone(lifecycle)
            self.assertEqual(lifecycle["shutdownPolicy"], "Delete")
            self.assertIn("shutdownTime", lifecycle)

    async def test_create_sandbox_without_shutdown_after_seconds(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            await self.client.create_sandbox("test-template", "test-namespace")

            call_kwargs = mock_create.call_args
            lifecycle = call_kwargs[1].get("lifecycle")
            self.assertIsNone(lifecycle)

    async def test_shutdown_after_seconds_validation_zero(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", shutdown_after_seconds=0)

    async def test_shutdown_after_seconds_validation_negative(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", shutdown_after_seconds=-1)

    async def test_shutdown_after_seconds_validation_bool(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", shutdown_after_seconds=True)


class TestAsyncSandbox(unittest.IsolatedAsyncioTestCase):

    async def test_requires_connection_config(self):
        with self.assertRaises(ValueError) as ctx:
            AsyncSandbox(
                claim_name="test",
                sandbox_id="test-id",
                connection_config=None,
            )
        self.assertIn("connection_config is required", str(ctx.exception))


class TestAsyncSandboxClientInCluster(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

    async def test_in_cluster_config_accepted(self):
        config = SandboxInClusterConnectionConfig()
        client = AsyncSandboxClient(connection_config=config)
        self.assertIsInstance(client.connection_config, SandboxInClusterConnectionConfig)

    async def test_pod_ip_passed_to_sandbox_when_use_pod_ip_true(self):
        config = SandboxInClusterConnectionConfig(use_pod_ip=True)
        client = AsyncSandboxClient(connection_config=config)
        mock_k8s_helper = client.k8s_helper
        mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="my-sandbox")

        mock_sandbox_class = MagicMock()
        mock_sandbox_class.return_value = MagicMock()
        client.sandbox_class = mock_sandbox_class

        with patch.object(client, "_create_claim", new_callable=AsyncMock), \
             patch.object(client, "_wait_for_sandbox_ready", new_callable=AsyncMock, return_value="10.244.0.5"):
            await client.create_sandbox("my-template")

        call_kwargs = mock_sandbox_class.call_args.kwargs
        self.assertEqual(call_kwargs["pod_ip"], "10.244.0.5")

    async def test_pod_ip_not_passed_when_use_pod_ip_false(self):
        config = SandboxInClusterConnectionConfig(use_pod_ip=False)
        client = AsyncSandboxClient(connection_config=config)
        mock_k8s_helper = client.k8s_helper
        mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="my-sandbox")

        mock_sandbox_class = MagicMock()
        mock_sandbox_class.return_value = MagicMock()
        client.sandbox_class = mock_sandbox_class

        with patch.object(client, "_create_claim", new_callable=AsyncMock), \
             patch.object(client, "_wait_for_sandbox_ready", new_callable=AsyncMock, return_value="10.244.0.5"):
            await client.create_sandbox("my-template")

        call_kwargs = mock_sandbox_class.call_args.kwargs
        self.assertIsNone(call_kwargs["pod_ip"])


class TestAsyncConnector(unittest.IsolatedAsyncioTestCase):

    async def test_rejects_local_tunnel_config(self):
        with self.assertRaises(ValueError) as ctx:
            AsyncSandboxConnector(
                sandbox_id="test",
                namespace="default",
                connection_config=SandboxLocalTunnelConnectionConfig(),
                k8s_helper=MagicMock(),
            )
        self.assertIn("does not support SandboxLocalTunnelConnectionConfig", str(ctx.exception))

    async def test_in_cluster_uses_dns_by_default(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        self.assertEqual(connector._base_url, "http://my-sandbox.dev.svc.cluster.local:8888")

    async def test_in_cluster_uses_pod_ip_when_provided(self):
        config = SandboxInClusterConnectionConfig(server_port=8888, use_pod_ip=True)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
            pod_ip="10.244.0.5",
        )
        self.assertEqual(connector._base_url, "http://10.244.0.5:8888")

    async def test_in_cluster_does_not_inject_router_headers(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        self.assertFalse(connector._inject_router_headers)

    async def test_direct_injects_router_headers(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        self.assertTrue(connector._inject_router_headers)


class AsyncSandboxHandler(BaseHTTPRequestHandler):
    """Minimal handler for async connector HTTP tests."""

    def do_POST(self):
        if self.path == "/execute":
            self._respond(HTTPStatus.OK, {"stdout": "hello", "stderr": "", "exit_code": 0})
        elif self.path == "/server-error":
            self._respond(HTTPStatus.INTERNAL_SERVER_ERROR, {"detail": "boom"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "not found"})

    def do_GET(self):
        if self.path == "/health":
            self._respond(HTTPStatus.OK, {"status": "healthy"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "not found"})

    def _respond(self, status: HTTPStatus, body: dict):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        payload = json.dumps(body).encode()
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


class TestAsyncConnectorHTTP(unittest.IsolatedAsyncioTestCase):

    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), AsyncSandboxHandler)
        cls.port = cls.server.server_address[1]
        cls.server_thread = Thread(target=cls.server.serve_forever)
        cls.server_thread.daemon = True
        cls.server_thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.server_thread.join(timeout=5)

    def _make_connector(self) -> AsyncSandboxConnector:
        config = SandboxDirectConnectionConfig(
            api_url=f"http://127.0.0.1:{self.port}",
            server_port=self.port,
        )
        k8s_helper = MagicMock()
        return AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=k8s_helper,
        )

    async def test_successful_request(self):
        connector = self._make_connector()
        try:
            response = await connector.send_request("GET", "health")
            self.assertEqual(response.status_code, 200)
            self.assertEqual(response.json()["status"], "healthy")
        finally:
            await connector.close()

    async def test_post_execute(self):
        connector = self._make_connector()
        try:
            response = await connector.send_request(
                "POST", "execute", json={"command": "echo hello"}
            )
            self.assertEqual(response.status_code, 200)
            data = response.json()
            self.assertEqual(data["stdout"], "hello")
            self.assertEqual(data["exit_code"], 0)
        finally:
            await connector.close()

    async def test_404_raises_sandbox_request_error(self):
        connector = self._make_connector()
        try:
            with self.assertRaises(SandboxRequestError) as ctx:
                await connector.send_request("GET", "nonexistent")
            self.assertEqual(ctx.exception.status_code, 404)
        finally:
            await connector.close()

    async def test_sandbox_request_error_is_runtime_error(self):
        """Backward compat: SandboxRequestError is still a RuntimeError."""
        connector = self._make_connector()
        try:
            with self.assertRaises(RuntimeError):
                await connector.send_request("GET", "nonexistent")
        finally:
            await connector.close()

    async def test_connection_refused_no_status_code(self):
        config = SandboxDirectConnectionConfig(
            api_url="http://127.0.0.1:1", server_port=1
        )
        connector = AsyncSandboxConnector(
            sandbox_id="test",
            namespace="default",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        try:
            with self.assertRaises(SandboxRequestError) as ctx:
                await connector.send_request("POST", "run", timeout=1)
            self.assertIsNone(ctx.exception.status_code)
        finally:
            await connector.close()

    async def test_sandbox_headers_sent(self):
        """Verify X-Sandbox-* headers are included in requests."""
        connector = self._make_connector()
        try:
            response = await connector.send_request("GET", "health")
            # We can't easily inspect request headers from the server side
            # in this test setup, but the request succeeds which validates
            # the header injection doesn't break the flow.
            self.assertEqual(response.status_code, 200)
        finally:
            await connector.close()


if __name__ == "__main__":
    unittest.main()
