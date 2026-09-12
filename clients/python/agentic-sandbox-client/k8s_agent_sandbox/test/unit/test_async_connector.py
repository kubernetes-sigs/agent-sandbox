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

"""Tests for async sandboxd tunnel and gRPC channel ownership."""

import asyncio
import sys
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

from k8s_agent_sandbox.async_connector import (
    AsyncSandboxConnector,
    AsyncSandboxdPodTunnelStrategy,
)
from k8s_agent_sandbox.exceptions import SandboxPortForwardError
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig


class TestAsyncSandboxdConnector(unittest.IsolatedAsyncioTestCase):
    """Exercise sandboxd connection setup, reuse, failures, and cleanup."""

    def _build(self):
        helper = MagicMock()
        return AsyncSandboxConnector(
            sandbox_id="sandbox-1",
            namespace="agents",
            connection_config=SandboxdPodTunnelConnectionConfig(
                rest_port=8080,
                grpc_port=9090,
            ),
            k8s_helper=helper,
            get_pod_name=AsyncMock(return_value="sandbox-1"),
        )

    async def test_sandboxd_config_is_supported(self):
        connector = self._build()
        self.assertTrue(connector.is_sandboxd())
        self.assertFalse(connector.should_inject_router_headers())
        await connector.close()

    async def test_connect_exposes_rest_and_grpc_endpoints(self):
        connector = self._build()
        connector._sandboxd_strategy.connect = AsyncMock(
            return_value=("http://127.0.0.1:18080", "127.0.0.1:19090")
        )
        try:
            base_url = await connector.connect()

            self.assertEqual(base_url, "http://127.0.0.1:18080")
            self.assertEqual(connector.grpc_target, "127.0.0.1:19090")
        finally:
            await connector.close()

    async def test_close_closes_tunnel_and_grpc_channel(self):
        connector = self._build()
        connector._sandboxd_strategy.close = AsyncMock()
        channel = MagicMock()
        channel.close = AsyncMock()
        connector._grpc_channel = channel

        await connector.close()

        connector._sandboxd_strategy.close.assert_awaited_once()
        channel.close.assert_awaited_once()
        self.assertIsNone(connector._grpc_channel)

    async def test_concurrent_grpc_channel_creates_one_channel(self):
        connector = self._build()
        connector._sandboxd_strategy.connect = AsyncMock(
            return_value=("http://127.0.0.1:18080", "127.0.0.1:19090")
        )
        channel = MagicMock()
        insecure_channel = MagicMock(return_value=channel)
        fake_grpc = SimpleNamespace(
            aio=SimpleNamespace(insecure_channel=insecure_channel)
        )

        with patch.dict(sys.modules, {"grpc": fake_grpc}):
            first, second = await asyncio.gather(
                connector.grpc_channel(), connector.grpc_channel()
            )

        self.assertIs(first, second)
        insecure_channel.assert_called_once_with("127.0.0.1:19090")
        await connector.close()

    async def test_missing_grpc_does_not_start_tunnel(self):
        connector = self._build()
        connector._sandboxd_strategy.connect = AsyncMock()

        try:
            with patch.dict(sys.modules, {"grpc": None}):
                with self.assertRaisesRegex(
                    ImportError, "pip install k8s-agent-sandbox\\[grpc\\]"
                ):
                    await connector.grpc_channel()
            connector._sandboxd_strategy.connect.assert_not_awaited()
        finally:
            await connector.close()

    async def test_connector_rejects_connect_after_close(self):
        connector = self._build()
        await connector.close()

        with self.assertRaisesRegex(RuntimeError, "closed"):
            await connector.connect()

    def test_connector_atexit_cleanup_releases_state_without_async_closes(self):
        """Connector atexit cleanup delegates only to the synchronous tunnel path."""
        connector = self._build()
        connector._sandboxd_strategy._close_for_atexit = MagicMock()
        connector.grpc_target = "127.0.0.1:19090"
        connector._grpc_channel = MagicMock()
        connector._grpc_channel_target = connector.grpc_target
        connector._base_url = "http://127.0.0.1:18080"

        connector._close_for_atexit()

        connector._sandboxd_strategy._close_for_atexit.assert_called_once_with()
        self.assertTrue(connector._closed)
        self.assertIsNone(connector.grpc_target)
        self.assertIsNone(connector._grpc_channel)
        self.assertIsNone(connector._grpc_channel_target)
        self.assertIsNone(connector._base_url)

    @patch("k8s_agent_sandbox.async_connector.asyncio.create_subprocess_exec")
    @patch.object(AsyncSandboxdPodTunnelStrategy, "_is_port_open", new_callable=AsyncMock)
    @patch.object(AsyncSandboxdPodTunnelStrategy, "_get_free_port")
    async def test_tunnel_forwards_rest_and_grpc_ports(
        self, get_free_port, is_port_open, create_subprocess
    ):
        process = MagicMock(returncode=None)
        process.terminate = MagicMock()
        process.wait = AsyncMock()
        create_subprocess.return_value = process
        get_free_port.side_effect = [18080, 19090]
        is_port_open.return_value = True
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(
                rest_port=8080,
                grpc_port=9090,
            ),
            get_pod_name=AsyncMock(return_value="sandbox-1"),
        )

        base_url, grpc_target = await strategy.connect()

        self.assertEqual(base_url, "http://127.0.0.1:18080")
        self.assertEqual(grpc_target, "127.0.0.1:19090")
        create_subprocess.assert_awaited_once_with(
            "kubectl",
            "port-forward",
            "pod/sandbox-1",
            "18080:8080",
            "19090:9090",
            "-n",
            "agents",
            stdout=asyncio.subprocess.DEVNULL,
            stderr=asyncio.subprocess.DEVNULL,
        )
        await strategy.close()

    @patch("k8s_agent_sandbox.async_connector.asyncio.create_subprocess_exec")
    @patch.object(AsyncSandboxdPodTunnelStrategy, "_is_port_open", new_callable=AsyncMock)
    @patch.object(AsyncSandboxdPodTunnelStrategy, "_get_free_port")
    async def test_concurrent_connect_starts_one_tunnel(
        self, get_free_port, is_port_open, create_subprocess
    ):
        process = MagicMock(returncode=None)
        process.terminate = MagicMock()
        process.wait = AsyncMock()
        create_subprocess.return_value = process
        get_free_port.side_effect = [18080, 19090]
        is_port_open.return_value = True
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
            get_pod_name=AsyncMock(return_value="sandbox-1"),
        )

        first, second = await asyncio.gather(strategy.connect(), strategy.connect())

        self.assertEqual(first, second)
        create_subprocess.assert_awaited_once()
        await strategy.close()

    @patch(
        "k8s_agent_sandbox.async_connector.asyncio.create_subprocess_exec",
        new_callable=AsyncMock,
    )
    async def test_tunnel_start_error_includes_pod_context(self, create_subprocess):
        create_subprocess.side_effect = FileNotFoundError("kubectl not found")
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
            get_pod_name=AsyncMock(return_value="sandbox-1"),
        )

        with self.assertRaisesRegex(
            SandboxPortForwardError, "sandbox-1.*agents|agents.*sandbox-1"
        ):
            await strategy.connect()

    @patch("k8s_agent_sandbox.async_connector.asyncio.create_subprocess_exec")
    async def test_tunnel_timeout_uses_port_forward_error(self, create_subprocess):
        process = MagicMock(returncode=None)
        process.terminate = MagicMock()
        process.wait = AsyncMock()
        create_subprocess.return_value = process
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(port_forward_ready_timeout=0),
            get_pod_name=AsyncMock(return_value="sandbox-1"),
        )

        with self.assertRaisesRegex(
            SandboxPortForwardError, "sandbox-1.*agents|agents.*sandbox-1"
        ):
            await strategy.connect()

    async def test_tunnel_does_not_restart_after_close(self):
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
            get_pod_name=AsyncMock(return_value="sandbox-1"),
        )

        await strategy.close()

        with self.assertRaisesRegex(SandboxPortForwardError, "closed"):
            await strategy.connect()

    def test_atexit_cleanup_terminates_process_without_waiting(self):
        """The atexit path must not await a process bound to another loop."""
        process = MagicMock(returncode=None)
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
        )
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"
        strategy.grpc_target = "127.0.0.1:19090"

        strategy._close_for_atexit()

        process.terminate.assert_called_once_with()
        process.kill.assert_called_once_with()
        process.wait.assert_not_called()
        self.assertIsNone(strategy.port_forward_process)
        self.assertIsNone(strategy.base_url)
        self.assertIsNone(strategy.grpc_target)


if __name__ == "__main__":
    unittest.main()
