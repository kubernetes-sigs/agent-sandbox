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

import httpx

from k8s_agent_sandbox.async_connector import (
    AsyncSandboxConnector,
    AsyncSandboxdInClusterStrategy,
    AsyncSandboxdPodTunnelStrategy,
)
from k8s_agent_sandbox.exceptions import (
    SandboxNotReadyError,
    SandboxPortForwardError,
    SandboxRequestError,
    SandboxServiceUnavailableError,
)
from k8s_agent_sandbox.models import (
    SandboxdInClusterConnectionConfig,
    SandboxdPodTunnelConnectionConfig,
)


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

    async def test_close_reaps_resources_when_http_client_close_fails(self):
        connector = self._build()
        connector.client.aclose = AsyncMock(side_effect=RuntimeError("http close failed"))
        connector._sandboxd_strategy.close = AsyncMock()
        channel = MagicMock()
        channel.close = AsyncMock()
        connector._grpc_channel = channel

        with self.assertRaisesRegex(RuntimeError, "http close failed"):
            await connector.close()

        connector._sandboxd_strategy.close.assert_awaited_once()
        channel.close.assert_awaited_once()

    async def test_close_reaps_tunnel_when_channel_close_fails_and_retries(self):
        connector = self._build()
        channel = MagicMock()
        channel.close = AsyncMock(side_effect=[RuntimeError("channel close failed"), None])
        connector._grpc_channel = channel
        connector._sandboxd_strategy.close = AsyncMock(side_effect=[None, None])

        with self.assertRaisesRegex(RuntimeError, "channel close failed"):
            await connector.close()

        connector._sandboxd_strategy.close.assert_awaited_once()
        self.assertIs(connector._grpc_channel, channel)

        await connector.close()

        self.assertIsNone(connector._grpc_channel)
        self.assertEqual(connector._sandboxd_strategy.close.await_count, 2)

    async def test_close_preserves_first_error_after_strategy_cleanup_fails(self):
        connector = self._build()
        connector.client.aclose = AsyncMock(side_effect=RuntimeError("http close failed"))
        connector._sandboxd_strategy.close = AsyncMock(
            side_effect=RuntimeError("tunnel close failed")
        )

        with self.assertRaisesRegex(RuntimeError, "http close failed"):
            await connector.close()

        connector._sandboxd_strategy.close.assert_awaited_once()
        self.assertFalse(connector._close_complete)

    async def test_close_prioritizes_cancellation_over_other_cleanup_errors(self):
        connector = self._build()
        connector.client.aclose = AsyncMock(side_effect=RuntimeError("http close failed"))
        connector._sandboxd_strategy.close = AsyncMock(
            side_effect=asyncio.CancelledError()
        )

        with self.assertRaises(asyncio.CancelledError):
            await connector.close()

        connector._sandboxd_strategy.close.assert_awaited_once()
        self.assertFalse(connector._close_complete)

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

    async def test_tunnel_close_retains_process_for_retry_after_wait_failure(self):
        process = MagicMock(returncode=None)
        process.terminate = MagicMock()
        process.wait = AsyncMock(side_effect=[RuntimeError("wait failed"), None])
        strategy = AsyncSandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
        )
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"
        strategy.grpc_target = "127.0.0.1:19090"

        with self.assertRaisesRegex(RuntimeError, "wait failed"):
            await strategy.close()

        self.assertIs(strategy.port_forward_process, process)
        await strategy.close()
        self.assertIsNone(strategy.port_forward_process)


class TestAsyncSandboxdInClusterConnector(unittest.IsolatedAsyncioTestCase):
    def _build(self, mode="service-dns", pod_ip=None, service_fqdn=None,
               rest_port=8080, grpc_port=9090):
        pod_ip = pod_ip or AsyncMock(return_value="10.0.0.1")
        service_fqdn = service_fqdn or AsyncMock(
            return_value="sandbox.agents.svc.example.internal"
        )
        connector = AsyncSandboxConnector(
            sandbox_id="sandbox",
            namespace="agents",
            connection_config=SandboxdInClusterConnectionConfig(
                mode=mode, rest_port=rest_port, grpc_port=grpc_port
            ),
            k8s_helper=MagicMock(),
            get_pod_ip=pod_ip,
            get_service_fqdn=service_fqdn,
        )
        return connector, pod_ip, service_fqdn

    async def test_service_mode_uses_reported_fqdn_and_caches_it(self):
        connector, pod_ip, service = self._build(
            rest_port=18080, grpc_port=19090
        )
        self.assertIsInstance(connector._sandboxd_strategy, AsyncSandboxdInClusterStrategy)
        self.assertTrue(connector.is_sandboxd())
        self.assertFalse(connector.should_inject_router_headers())
        try:
            self.assertEqual(
                await connector.connect(),
                "http://sandbox.agents.svc.example.internal:18080",
            )
            self.assertEqual(
                connector.grpc_target,
                "sandbox.agents.svc.example.internal:19090",
            )
            await connector.connect()
            service.assert_awaited_once()
            pod_ip.assert_not_awaited()
        finally:
            await connector.close()

    async def test_service_mode_missing_fqdn_never_falls_back(self):
        connector, pod_ip, _ = self._build(
            service_fqdn=AsyncMock(return_value=None)
        )
        try:
            with self.assertRaisesRegex(SandboxServiceUnavailableError, "spec.service"):
                await connector.connect()
            pod_ip.assert_not_awaited()
        finally:
            await connector.close()

    async def test_pod_mode_refreshes_ip_and_brackets_ipv6(self):
        pod_ip = AsyncMock(side_effect=["10.0.0.1", "2001:db8::5"])
        connector, _, service = self._build(
            mode="pod-ip", pod_ip=pod_ip, rest_port=18080, grpc_port=19090
        )
        try:
            self.assertEqual(await connector.connect(), "http://10.0.0.1:18080")
            self.assertEqual(await connector.connect(), "http://[2001:db8::5]:18080")
            self.assertEqual(connector.grpc_target, "[2001:db8::5]:19090")
            service.assert_not_awaited()
            self.assertEqual(pod_ip.await_count, 2)
        finally:
            await connector.close()

    async def test_pod_mode_missing_ip_and_status_error_fail_closed(self):
        pod_ip = AsyncMock(
            side_effect=["10.0.0.1", PermissionError("status denied"), None]
        )
        connector, _, service = self._build(mode="pod-ip", pod_ip=pod_ip)
        try:
            await connector.connect()
            with self.assertRaisesRegex(PermissionError, "status denied"):
                await connector.connect()
            self.assertIsNone(connector.grpc_target)
            with self.assertRaises(SandboxNotReadyError):
                await connector.connect()
            service.assert_not_awaited()
        finally:
            await connector.close()

    @patch("k8s_agent_sandbox.async_connector.asyncio.create_subprocess_exec")
    async def test_rest_request_has_no_router_headers_or_subprocess(self, subprocess_exec):
        connector, _, _ = self._build(mode="pod-ip")
        response = httpx.Response(
            200, request=httpx.Request("GET", "http://10.0.0.1:8080/v1/files/a.txt")
        )
        connector.client.request = AsyncMock(return_value=response)
        try:
            await connector.send_request("GET", "v1/files/a.txt")
            args, kwargs = connector.client.request.call_args
            self.assertEqual(args[1], "http://10.0.0.1:8080/v1/files/a.txt")
            self.assertFalse(any(key.startswith("X-Sandbox-") for key in kwargs["headers"]))
            subprocess_exec.assert_not_awaited()
        finally:
            await connector.close()

    async def test_grpc_channel_reuse_replacement_and_close(self):
        pod_ip = AsyncMock(side_effect=["10.0.0.1", "10.0.0.1", "10.0.0.2"])
        connector, _, _ = self._build(mode="pod-ip", pod_ip=pod_ip)
        first, second = MagicMock(), MagicMock()
        first.close = AsyncMock()
        second.close = AsyncMock()
        dial = MagicMock(side_effect=[first, second])
        fake_grpc = SimpleNamespace(aio=SimpleNamespace(insecure_channel=dial))
        with patch.dict(sys.modules, {"grpc": fake_grpc}):
            await connector.connect()
            self.assertIs(await connector.grpc_channel(), first)
            await connector.connect()
            self.assertIs(await connector.grpc_channel(), first)
            await connector.connect()
            self.assertIs(await connector.grpc_channel(), second)
        self.assertEqual(dial.call_count, 2)
        dial.assert_any_call("10.0.0.1:9090")
        dial.assert_any_call("10.0.0.2:9090")
        first.close.assert_awaited_once()
        await connector.close()
        second.close.assert_awaited_once()

    async def test_service_transport_failure_refreshes_fqdn_next_time(self):
        service = AsyncMock(side_effect=["old.agents.svc", "new.agents.svc"])
        connector, _, _ = self._build(service_fqdn=service)
        request = httpx.Request("GET", "http://old.agents.svc:8080/v1/files/a.txt")
        response = httpx.Response(200, request=request)
        connector.client.request = AsyncMock(
            side_effect=[httpx.ConnectError("dns failed", request=request), response]
        )
        try:
            with self.assertRaises(SandboxRequestError):
                await connector.send_request("GET", "v1/files/a.txt")
            await connector.send_request("GET", "v1/files/a.txt")
            self.assertEqual(service.await_count, 2)
            args, _ = connector.client.request.call_args
            self.assertIn("new.agents.svc", args[1])
        finally:
            await connector.close()

    async def test_service_transport_failure_discards_existing_grpc_channel(self):
        service = AsyncMock(return_value="stable.agents.svc")
        connector, _, _ = self._build(service_fqdn=service)
        first, second = MagicMock(), MagicMock()
        first.close = AsyncMock()
        second.close = AsyncMock()
        dial = MagicMock(side_effect=[first, second])
        request = httpx.Request("GET", "http://stable.agents.svc:8080/v1/files/a.txt")
        connector.client.request = AsyncMock(
            side_effect=httpx.ConnectError("connection failed", request=request)
        )
        fake_grpc = SimpleNamespace(aio=SimpleNamespace(insecure_channel=dial))
        try:
            with patch.dict(sys.modules, {"grpc": fake_grpc}):
                await connector.connect()
                self.assertIs(await connector.grpc_channel(), first)
                with self.assertRaises(SandboxRequestError):
                    await connector.send_request("GET", "v1/files/a.txt")
                first.close.assert_awaited_once()
                self.assertIsNone(connector._grpc_channel)
                await connector.connect()
                self.assertIs(await connector.grpc_channel(), second)
            self.assertEqual(service.await_count, 2)
            self.assertEqual(dial.call_count, 2)
        finally:
            await connector.close()

    async def test_pod_transport_failure_discards_existing_grpc_channel(self):
        connector, _, _ = self._build(mode="pod-ip")
        channel = MagicMock()
        channel.close = AsyncMock()
        request = httpx.Request("GET", "http://10.0.0.1:8080/v1/files/a.txt")
        connector.client.request = AsyncMock(
            side_effect=httpx.ConnectError("connection failed", request=request)
        )
        fake_grpc = SimpleNamespace(
            aio=SimpleNamespace(insecure_channel=MagicMock(return_value=channel))
        )
        try:
            with patch.dict(sys.modules, {"grpc": fake_grpc}):
                await connector.connect()
                self.assertIs(await connector.grpc_channel(), channel)
                with self.assertRaises(SandboxRequestError):
                    await connector.send_request("GET", "v1/files/a.txt")
            channel.close.assert_awaited_once()
            self.assertIsNone(connector._grpc_channel)
        finally:
            await connector.close()

    async def test_service_http_5xx_keeps_fqdn_cache(self):
        connector, _, service = self._build()
        request = httpx.Request("POST", "http://sandbox.agents.svc:8080/v1/files/a.txt")
        connector.client.request = AsyncMock(
            return_value=httpx.Response(500, request=request)
        )
        try:
            with self.assertRaises(SandboxRequestError):
                await connector.send_request("POST", "v1/files/a.txt")
            await connector.connect()
            service.assert_awaited_once()
        finally:
            await connector.close()

    async def test_grpc_unavailable_discards_channel_and_service_cache(self):
        service = AsyncMock(side_effect=["old.agents.svc", "new.agents.svc"])
        connector, _, _ = self._build(service_fqdn=service)
        channel = MagicMock()
        channel.close = AsyncMock()
        fake_grpc = SimpleNamespace(
            aio=SimpleNamespace(insecure_channel=MagicMock(return_value=channel))
        )
        with patch.dict(sys.modules, {"grpc": fake_grpc}):
            await connector.connect()
            self.assertIs(await connector.grpc_channel(), channel)
        await connector.invalidate_sandboxd_transport(channel)
        channel.close.assert_awaited_once()
        self.assertEqual(await connector.connect(), "http://new.agents.svc:8080")
        await connector.close()

    def test_atexit_path_clears_direct_state_without_subprocess(self):
        connector, _, _ = self._build()
        strategy = connector._sandboxd_strategy
        self.assertIsInstance(strategy, AsyncSandboxdInClusterStrategy)
        strategy._service_fqdn = "sandbox.agents.svc"
        connector.grpc_target = "sandbox.agents.svc:9090"
        connector._close_for_atexit()
        self.assertTrue(connector._closed)
        self.assertIsNone(connector.grpc_target)
        self.assertIsNone(strategy._service_fqdn)


if __name__ == "__main__":
    unittest.main()
