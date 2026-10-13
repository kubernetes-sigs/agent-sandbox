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

"""Unit tests for synchronous sandbox connectivity."""

import io
import json
import os
import subprocess
import sys
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, HTTPServer
from types import SimpleNamespace
from unittest.mock import MagicMock, call, patch

import requests
from kubernetes import client as k8s_client
from pydantic import ValidationError

from k8s_agent_sandbox.connector import (
    DirectConnectionStrategy,
    GatewayConnectionStrategy,
    LocalTunnelConnectionStrategy,
    InClusterConnectionStrategy,
    SandboxdPodTunnelStrategy,
    SandboxdInClusterStrategy,
    SandboxConnector,
)
from k8s_agent_sandbox.exceptions import (
    SandboxNoServiceError,
    SandboxNotReadyError,
    SandboxPortForwardError,
    SandboxRequestError,
)
from k8s_agent_sandbox.models import (
    SandboxdPodTunnelConnectionConfig,
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxdPodTunnelConnectionConfig,
    SandboxdInClusterConnectionConfig,
    SandboxInClusterConnectionConfig,
)


class TestInClusterConnectionStrategy(unittest.TestCase):
    """Unit tests for InClusterConnectionStrategy."""

    def setUp(self):
        self.config = SandboxInClusterConnectionConfig(server_port=8888)
        self.strategy = InClusterConnectionStrategy(
            sandbox_id="my-sandbox",
            namespace="dev",
            config=self.config,
        )

    def test_connect_returns_correct_dns_url(self):
        url = self.strategy.connect()
        self.assertEqual(url, "http://my-sandbox.dev.svc.cluster.local:8888")

    def test_connect_uses_custom_port(self):
        config = SandboxInClusterConnectionConfig(server_port=9000)
        strategy = InClusterConnectionStrategy("sb", "ns", config)
        self.assertEqual(strategy.connect(), "http://sb.ns.svc.cluster.local:9000")

    def test_connect_is_idempotent(self):
        self.assertEqual(self.strategy.connect(), self.strategy.connect())

    def test_does_not_inject_router_headers(self):
        self.assertFalse(self.strategy.should_inject_router_headers())

    def test_verify_connection_does_not_raise(self):
        self.strategy.verify_connection()

    def test_close_does_not_raise(self):
        self.strategy.close()

    def test_connect_uses_pod_ip_when_callable_provided(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sandbox", "dev", config, get_pod_ip=lambda: "10.244.0.5")
        self.assertEqual(strategy.connect(), "http://10.244.0.5:8888")

    def test_connect_falls_back_to_dns_when_callable_returns_none(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sandbox", "dev", config, get_pod_ip=lambda: None)
        self.assertEqual(strategy.connect(), "http://my-sandbox.dev.svc.cluster.local:8888")

    def test_connect_uses_dns_when_no_callable(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sandbox", "dev", config, get_pod_ip=None)
        self.assertEqual(strategy.connect(), "http://my-sandbox.dev.svc.cluster.local:8888")

    def test_connect_pod_ip_uses_custom_port(self):
        config = SandboxInClusterConnectionConfig(server_port=9000)
        strategy = InClusterConnectionStrategy("sb", "ns", config, get_pod_ip=lambda: "192.168.1.1")
        self.assertEqual(strategy.connect(), "http://192.168.1.1:9000")

    def test_connect_caches_pod_ip_until_close(self):
        """Pod IP is cached across connect() calls; close() invalidates the cache."""
        ips = iter(["10.0.0.1", "10.0.0.2"])
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("sb", "ns", config, get_pod_ip=lambda: next(ips))
        self.assertEqual(strategy.connect(), "http://10.0.0.1:8888")
        self.assertEqual(strategy.connect(), "http://10.0.0.1:8888")  # cached
        strategy.close()  # invalidates cache
        self.assertEqual(strategy.connect(), "http://10.0.0.2:8888")  # fresh resolve

    def test_connect_brackets_ipv6_pod_ip(self):
        """IPv6 pod IPs must be enclosed in brackets in URLs (RFC 3986)."""
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy(
            "my-sandbox", "dev", config, get_pod_ip=lambda: "2001:db8::1"
        )
        self.assertEqual(strategy.connect(), "http://[2001:db8::1]:8888")


class TestSandboxdInClusterConnection(unittest.TestCase):
    def _build(self, mode="in-cluster-service", rest_port=8080, grpc_port=9090,
               pod_ip=None, service_fqdn=None):
        pod_ip = pod_ip or MagicMock(return_value="10.0.0.1")
        service_fqdn = service_fqdn or MagicMock(
            return_value="sandbox.agents.svc.example.internal"
        )
        connector = SandboxConnector(
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

    def test_service_mode_uses_reported_fqdn_for_both_endpoints(self):
        connector, pod_ip, service = self._build(
            rest_port=18080, grpc_port=19090
        )
        self.assertIsInstance(connector.strategy, SandboxdInClusterStrategy)
        self.assertTrue(connector.is_sandboxd())
        self.assertEqual(
            connector.connect(), "http://sandbox.agents.svc.example.internal:18080"
        )
        self.assertEqual(
            connector.strategy.grpc_target,
            "sandbox.agents.svc.example.internal:19090",
        )
        self.assertEqual(connector.connect(), connector.connect())
        service.assert_called_once()
        pod_ip.assert_not_called()
        connector.close()

    def test_service_mode_missing_fqdn_never_falls_back(self):
        connector, pod_ip, _ = self._build(
            service_fqdn=MagicMock(return_value=None)
        )
        with self.assertRaisesRegex(SandboxNoServiceError, "spec.service"):
            connector.connect()
        pod_ip.assert_not_called()
        connector.close()

    def test_pod_mode_refreshes_ip_and_brackets_ipv6(self):
        pod_ip = MagicMock(side_effect=["10.0.0.1", "2001:db8::5"])
        connector, _, service = self._build(
            mode="in-cluster-pod-ip", pod_ip=pod_ip, rest_port=18080, grpc_port=19090
        )
        self.assertEqual(connector.connect(), "http://10.0.0.1:18080")
        self.assertEqual(connector.connect(), "http://[2001:db8::5]:18080")
        self.assertEqual(connector.strategy.grpc_target, "[2001:db8::5]:19090")
        service.assert_not_called()
        self.assertEqual(pod_ip.call_count, 2)
        connector.close()

    def test_pod_mode_missing_ip_never_falls_back(self):
        connector, _, service = self._build(
            mode="in-cluster-pod-ip", pod_ip=MagicMock(return_value=None)
        )
        with self.assertRaises(SandboxNotReadyError):
            connector.connect()
        service.assert_not_called()
        connector.close()

    def test_pod_status_read_error_does_not_reuse_old_target(self):
        pod_ip = MagicMock(side_effect=["10.0.0.1", PermissionError("status denied")])
        connector, _, _ = self._build(mode="in-cluster-pod-ip", pod_ip=pod_ip)
        connector.connect()
        with self.assertRaisesRegex(PermissionError, "status denied"):
            connector.connect()
        self.assertIsNone(connector.strategy.grpc_target)
        connector.close()

    @patch("k8s_agent_sandbox.connector.subprocess.Popen")
    def test_rest_request_uses_direct_endpoint_without_router_headers(self, popen):
        connector, _, _ = self._build(mode="in-cluster-pod-ip")
        response = MagicMock(spec=requests.Response)
        response.status_code = 200
        response.is_redirect = False
        connector.session.request = MagicMock(return_value=response)
        connector.send_request("GET", "v1/files/a.txt", timeout=5)
        args, kwargs = connector.session.request.call_args
        self.assertEqual(args[1], "http://10.0.0.1:8080/v1/files/a.txt")
        self.assertFalse(kwargs["allow_redirects"])
        self.assertFalse(any(key.startswith("X-Sandbox-") for key in kwargs["headers"]))
        popen.assert_not_called()
        connector.close()

    def test_grpc_channel_reuses_target_and_replaces_changed_ip(self):
        pod_ip = MagicMock(side_effect=["10.0.0.1", "10.0.0.1", "10.0.0.2"])
        connector, _, _ = self._build(mode="in-cluster-pod-ip", pod_ip=pod_ip)
        first, second = MagicMock(), MagicMock()
        dial = MagicMock(side_effect=[first, second])
        with patch.dict(sys.modules, {"grpc": SimpleNamespace(insecure_channel=dial)}):
            connector.connect()
            self.assertIs(connector.grpc_channel(), first)
            connector.connect()
            self.assertIs(connector.grpc_channel(), first)
            connector.connect()
            self.assertIs(connector.grpc_channel(), second)
        self.assertEqual(dial.call_count, 2)
        dial.assert_any_call("10.0.0.1:9090")
        dial.assert_any_call("10.0.0.2:9090")
        first.close.assert_called_once()
        connector.close()
        second.close.assert_called_once()

    def test_service_transport_failure_refreshes_fqdn_on_next_request(self):
        service = MagicMock(side_effect=["old.agents.svc", "new.agents.svc"])
        connector, _, _ = self._build(service_fqdn=service)
        response = MagicMock(spec=requests.Response)
        response.status_code = 200
        response.is_redirect = False
        connector.session.request = MagicMock(
            side_effect=[requests.ConnectionError("dns failed"), response]
        )
        with self.assertRaises(SandboxRequestError):
            connector.send_request("GET", "v1/files/a.txt")
        connector.send_request("GET", "v1/files/a.txt")
        self.assertEqual(service.call_count, 2)
        args, _ = connector.session.request.call_args
        self.assertIn("new.agents.svc", args[1])
        connector.close()

    def test_service_http_5xx_keeps_fqdn_cache(self):
        connector, _, service = self._build()
        response = MagicMock(spec=requests.Response)
        response.status_code = 500
        response.is_redirect = False
        response.raise_for_status.side_effect = requests.HTTPError(response=response)
        connector.session.request = MagicMock(return_value=response)
        with self.assertRaises(SandboxRequestError):
            connector.send_request("GET", "v1/files/a.txt")
        connector.connect()
        service.assert_called_once()
        connector.close()

    def test_grpc_unavailable_discards_channel_and_service_cache(self):
        service = MagicMock(side_effect=["old.agents.svc", "new.agents.svc"])
        connector, _, _ = self._build(service_fqdn=service)
        channel = MagicMock()
        with patch.dict(
            sys.modules,
            {"grpc": SimpleNamespace(insecure_channel=MagicMock(return_value=channel))},
        ):
            connector.connect()
            self.assertIs(connector.grpc_channel(), channel)
        connector.invalidate_sandboxd_transport(channel)
        channel.close.assert_called_once()
        self.assertEqual(connector.connect(), "http://new.agents.svc:8080")
        connector.close()

    def test_pod_transport_failure_does_not_redial_stale_grpc_target(self):
        pod_ip = MagicMock(side_effect=["10.0.0.1", "10.0.0.2"])
        connector, _, _ = self._build(mode="in-cluster-pod-ip", pod_ip=pod_ip)
        first, second = MagicMock(), MagicMock()
        dial = MagicMock(side_effect=[first, second])
        with patch.dict(sys.modules, {"grpc": SimpleNamespace(insecure_channel=dial)}):
            connector.connect()
            self.assertIs(connector.grpc_channel(), first)
            connector.invalidate_sandboxd_transport(first)
            self.assertIsNone(connector.strategy.grpc_target)
            with self.assertRaisesRegex(SandboxRequestError, "call connect"):
                connector.grpc_channel()
            self.assertEqual(dial.call_count, 1)

            self.assertEqual(connector.connect(), "http://10.0.0.2:8080")
            self.assertIs(connector.grpc_channel(), second)
        first.close.assert_called_once()
        connector.close()
        second.close.assert_called_once()

    def test_stream_transport_failure_discards_channel_and_service_cache(self):
        service = MagicMock(side_effect=["old.agents.svc", "new.agents.svc"])
        connector, _, _ = self._build(service_fqdn=service)
        channel = MagicMock()
        with patch.dict(
            sys.modules,
            {"grpc": SimpleNamespace(insecure_channel=MagicMock(return_value=channel))},
        ):
            connector.connect()
            self.assertIs(connector.grpc_channel(), channel)
        connector.invalidate_sandboxd_transport(None)
        channel.close.assert_called_once()
        self.assertEqual(connector.connect(), "http://new.agents.svc:8080")
        connector.close()

    def test_late_stream_failure_does_not_discard_replacement(self):
        service = MagicMock(side_effect=["old.agents.svc", "new.agents.svc"])
        connector, _, _ = self._build(service_fqdn=service)
        first, second = MagicMock(), MagicMock()
        response = MagicMock(spec=requests.Response)
        response.status_code = 200
        response.is_redirect = False
        connector.session.request = MagicMock(return_value=response)
        with patch.dict(sys.modules, {"grpc": SimpleNamespace(
            insecure_channel=MagicMock(side_effect=[first, second])
        )}):
            connector.connect()
            self.assertIs(connector.grpc_channel(), first)
            connector.send_request("GET", "v1/files/a.txt", stream=True)
            old_token = response._sandboxd_transport_token
            connector.invalidate_sandboxd_transport(None, transport_token=old_token)
            connector.connect()
            self.assertIs(connector.grpc_channel(), second)
            connector.invalidate_sandboxd_transport(None, transport_token=old_token)
            self.assertIs(connector.grpc_channel(), second)
            self.assertEqual(connector.connect(), "http://new.agents.svc:8080")
        first.close.assert_called_once()
        second.close.assert_not_called()
        self.assertEqual(service.call_count, 2)
        connector.close()


class TestGatewayConnectionStrategy(unittest.TestCase):
    """Unit tests for GatewayConnectionStrategy."""

    def test_connect_brackets_ipv6(self):
        """Gateway IPv6 addresses must be bracketed in the base URL."""
        config = SandboxGatewayConnectionConfig(gateway_name="gw", gateway_namespace="default")
        mock_helper = MagicMock()
        mock_helper.wait_for_gateway_ip.return_value = "2001:db8::1"
        strategy = GatewayConnectionStrategy(config, k8s_helper=mock_helper)
        self.assertEqual(strategy.connect(), "http://[2001:db8::1]")

    def test_connect_does_not_bracket_ipv4(self):
        """Gateway IPv4 addresses must NOT be bracketed."""
        config = SandboxGatewayConnectionConfig(gateway_name="gw", gateway_namespace="default")
        mock_helper = MagicMock()
        mock_helper.wait_for_gateway_ip.return_value = "34.56.78.90"
        strategy = GatewayConnectionStrategy(config, k8s_helper=mock_helper)
        self.assertEqual(strategy.connect(), "http://34.56.78.90")


class TestExistingStrategiesDefaultHeaderInjection(unittest.TestCase):
    """Regression: existing strategies must still inject router headers by default."""

    def test_direct_injects_headers(self):
        s = DirectConnectionStrategy(SandboxDirectConnectionConfig(api_url="http://x"))
        self.assertTrue(s.should_inject_router_headers())

    def test_gateway_injects_headers(self):
        s = GatewayConnectionStrategy(
            SandboxGatewayConnectionConfig(gateway_name="gw"),
            k8s_helper=MagicMock(),
        )
        self.assertTrue(s.should_inject_router_headers())

    def test_local_tunnel_injects_headers(self):
        s = LocalTunnelConnectionStrategy(
            sandbox_id="s", namespace="ns",
            config=SandboxLocalTunnelConnectionConfig(),
        )
        self.assertTrue(s.should_inject_router_headers())


class TestPortForwardCleanup(unittest.TestCase):
    def test_local_tunnel_retains_process_when_terminate_fails(self):
        strategy = LocalTunnelConnectionStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxLocalTunnelConnectionConfig(),
        )
        process = MagicMock()
        process.terminate.side_effect = [RuntimeError("terminate failed"), None]
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"

        strategy.close()

        self.assertIs(strategy.port_forward_process, process)
        self.assertEqual(strategy.base_url, "http://127.0.0.1:18080")

        strategy.close()

        self.assertIsNone(strategy.port_forward_process)
        self.assertIsNone(strategy.base_url)

    def test_sandboxd_tunnel_retains_process_when_kill_fails(self):
        strategy = SandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
        )
        process = MagicMock()
        process.wait.side_effect = [
            subprocess.TimeoutExpired(cmd="kubectl", timeout=2),
            None,
        ]
        process.kill.side_effect = RuntimeError("kill failed")
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"
        strategy.grpc_target = "127.0.0.1:19090"

        strategy.close()

        self.assertIs(strategy.port_forward_process, process)
        self.assertEqual(strategy.base_url, "http://127.0.0.1:18080")
        self.assertEqual(strategy.grpc_target, "127.0.0.1:19090")

        strategy.close()

        self.assertIsNone(strategy.port_forward_process)
        self.assertIsNone(strategy.base_url)
        self.assertIsNone(strategy.grpc_target)

    def test_local_tunnel_retains_process_when_kill_wait_times_out(self):
        strategy = LocalTunnelConnectionStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxLocalTunnelConnectionConfig(),
        )
        process = MagicMock()
        process.wait.side_effect = [
            subprocess.TimeoutExpired(cmd="kubectl", timeout=2),
            subprocess.TimeoutExpired(cmd="kubectl", timeout=2),
            None,
        ]
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"

        strategy.close()

        self.assertEqual(
            process.wait.call_args_list,
            [call(timeout=2), call(timeout=2)],
        )
        self.assertIs(strategy.port_forward_process, process)
        self.assertEqual(strategy.base_url, "http://127.0.0.1:18080")

        strategy.close()

        self.assertIsNone(strategy.port_forward_process)
        self.assertIsNone(strategy.base_url)

    def test_sandboxd_tunnel_retains_process_when_kill_wait_times_out(self):
        strategy = SandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
        )
        process = MagicMock()
        process.wait.side_effect = [
            subprocess.TimeoutExpired(cmd="kubectl", timeout=2),
            subprocess.TimeoutExpired(cmd="kubectl", timeout=2),
            None,
        ]
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"
        strategy.grpc_target = "127.0.0.1:19090"

        strategy.close()

        self.assertEqual(
            process.wait.call_args_list,
            [call(timeout=2), call(timeout=2)],
        )
        self.assertIs(strategy.port_forward_process, process)
        self.assertEqual(strategy.base_url, "http://127.0.0.1:18080")
        self.assertEqual(strategy.grpc_target, "127.0.0.1:19090")

        strategy.close()

        self.assertIsNone(strategy.port_forward_process)
        self.assertIsNone(strategy.base_url)
        self.assertIsNone(strategy.grpc_target)

    @patch("k8s_agent_sandbox.connector.subprocess.Popen")
    def test_local_tunnel_does_not_overwrite_process_after_failed_cleanup(
        self, popen
    ):
        strategy = LocalTunnelConnectionStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxLocalTunnelConnectionConfig(),
        )
        process = MagicMock()
        process.poll.return_value = 1
        process.terminate.side_effect = RuntimeError("terminate failed")
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"

        with self.assertRaisesRegex(SandboxPortForwardError, "existing port-forward"):
            strategy.connect()

        popen.assert_not_called()
        self.assertIs(strategy.port_forward_process, process)

    @patch("k8s_agent_sandbox.connector.subprocess.Popen")
    def test_sandboxd_tunnel_does_not_overwrite_process_after_failed_cleanup(
        self, popen
    ):
        strategy = SandboxdPodTunnelStrategy(
            sandbox_id="sandbox-1",
            namespace="agents",
            config=SandboxdPodTunnelConnectionConfig(),
            get_pod_name=lambda: "sandbox-1",
        )
        process = MagicMock()
        process.poll.return_value = 1
        process.terminate.side_effect = RuntimeError("terminate failed")
        strategy.port_forward_process = process
        strategy.base_url = "http://127.0.0.1:18080"
        strategy.grpc_target = "127.0.0.1:19090"

        with self.assertRaisesRegex(
            SandboxPortForwardError, "existing sandboxd port-forward"
        ):
            strategy.connect()

        popen.assert_not_called()
        self.assertIs(strategy.port_forward_process, process)


class TestSandboxConnectorStrategySelection(unittest.TestCase):
    def _make_connector(self, config):
        return SandboxConnector(
            sandbox_id="sb",
            namespace="ns",
            connection_config=config,
            k8s_helper=MagicMock(),
        )

    def test_post_requests_are_not_retried(self):
        connector = self._make_connector(
            SandboxDirectConnectionConfig(api_url="http://router")
        )
        retry_policy = connector.session.get_adapter("http://").max_retries
        no_retry_policy = connector._no_retry_session.get_adapter("http://").max_retries

        self.assertEqual(
            set(retry_policy.allowed_methods), {"GET", "PUT", "DELETE"}
        )
        self.assertEqual(no_retry_policy.total, 0)

    def test_selects_in_cluster_strategy(self):
        config = SandboxInClusterConnectionConfig()
        connector = self._make_connector(config)
        self.assertIsInstance(connector.strategy, InClusterConnectionStrategy)

    def test_selects_direct_strategy(self):
        config = SandboxDirectConnectionConfig(api_url="http://x")
        connector = self._make_connector(config)
        self.assertIsInstance(connector.strategy, DirectConnectionStrategy)

    def test_raises_on_unknown_config_type(self):
        with self.assertRaises(ValueError):
            SandboxConnector(
                sandbox_id="sb",
                namespace="ns",
                connection_config=object(),
                k8s_helper=MagicMock(),
            )


class TestSandboxConnectorHeaderInjection(unittest.TestCase):
    def _make_connector_with_strategy(self, strategy, config):
        connector = SandboxConnector(
            sandbox_id="my-sb",
            namespace="my-ns",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        connector.strategy = strategy
        mock_session = MagicMock()
        connector.session = mock_session
        return connector, mock_session

    def _mock_ok_response(self):
        mock_resp = MagicMock(spec=requests.Response)
        mock_resp.status_code = 200
        mock_resp.is_redirect = False
        mock_resp.raise_for_status.return_value = None
        return mock_resp

    def test_router_headers_NOT_sent_for_in_cluster(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sb", "my-ns", config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute")

        call_args, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-ID", sent_headers)
        self.assertNotIn("X-Sandbox-Namespace", sent_headers)
        self.assertNotIn("X-Sandbox-Port", sent_headers)

    def test_router_headers_ARE_sent_for_direct(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute")

        call_args, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertIn("X-Sandbox-ID", sent_headers)
        self.assertIn("X-Sandbox-Namespace", sent_headers)
        self.assertIn("X-Sandbox-Port", sent_headers)

    def test_timeout_header_is_sent_for_router_requests(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute", timeout=123)

        _, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertEqual(sent_headers.get("X-Sandbox-Timeout"), "123")

    def test_timeout_tuple_uses_last_value_for_router_requests(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute", timeout=(3, 123))

        _, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertEqual(sent_headers.get("X-Sandbox-Timeout"), "123")

    def test_timeout_tuple_without_read_timeout_does_not_send_header(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute", timeout=(5, None))

        _, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-Timeout", sent_headers)

    def test_unsupported_timeout_does_not_send_header(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute", timeout=object())

        _, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-Timeout", sent_headers)

    def test_timeout_header_is_not_sent_for_in_cluster_requests(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sb", "my-ns", config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute", timeout=123)

        _, call_kwargs = mock_session.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-Timeout", sent_headers)

    def test_in_cluster_url_is_pod_dns(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sb", "my-ns", config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("POST", "execute")

        call_args, call_kwargs = mock_session.request.call_args
        url = call_args[1]
        self.assertEqual(url, "http://my-sb.my-ns.svc.cluster.local:8888/execute")

    def test_allow_redirects_is_false(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute")

        call_args, call_kwargs = mock_session.request.call_args
        self.assertFalse(call_kwargs.get("allow_redirects", True))

    def test_allow_redirects_in_kwargs_popped(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("GET", "/execute", allow_redirects=True)

        call_args, call_kwargs = mock_session.request.call_args
        self.assertFalse(call_kwargs.get("allow_redirects", True))

    def test_disable_retries_uses_pooled_one_shot_session(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_no_retry_session = MagicMock()
        connector._no_retry_session = mock_no_retry_session
        mock_no_retry_session.request.return_value = self._mock_ok_response()

        connector.send_request(
            "PUT", "/upload", data=iter([b"payload"]), _disable_retries=True
        )

        mock_session.request.assert_not_called()
        _, call_kwargs = mock_no_retry_session.request.call_args
        self.assertNotIn("_disable_retries", call_kwargs)
        self.assertFalse(call_kwargs["allow_redirects"])

    def test_redirect_raises_error(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)

        mock_resp = MagicMock(spec=requests.Response)
        mock_resp.status_code = 302
        mock_resp.is_redirect = True
        mock_resp.raise_for_status.return_value = None
        mock_session.request.return_value = mock_resp

        from k8s_agent_sandbox.connector import SandboxRequestError
        with self.assertRaises(SandboxRequestError):
            connector.send_request("GET", "/execute")

    def test_304_does_not_raise_redirect_error(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)

        mock_resp = MagicMock(spec=requests.Response)
        mock_resp.status_code = 304
        mock_resp.is_redirect = False
        mock_resp.raise_for_status.return_value = None
        mock_session.request.return_value = mock_resp

        connector.send_request("GET", "/execute")

    def test_300_does_not_raise_redirect_error(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        strategy = DirectConnectionStrategy(config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)

        mock_resp = MagicMock(spec=requests.Response)
        mock_resp.status_code = 300
        mock_resp.is_redirect = False
        mock_resp.raise_for_status.return_value = None
        mock_session.request.return_value = mock_resp

        connector.send_request("GET", "/execute")

class TestSandboxDirectConnectionConfigExtras(unittest.TestCase):
    """Validation of extra_headers and the mTLS options."""

    def test_reserved_routing_header_is_rejected_case_insensitively(self):
        for name in ("X-Sandbox-ID", "x-sandbox-port", "X-SANDBOX-Timeout"):
            with self.subTest(name=name):
                with self.assertRaisesRegex(ValidationError, "reserved"):
                    SandboxDirectConnectionConfig(
                        api_url="https://router", extra_headers={name: "v"}
                    )

    def test_tls_options_require_https(self):
        for options in ({"ca_cert": "/ca.pem"}, {"client_cert": ("/c.crt", "/c.key")}):
            with self.subTest(options=options):
                with self.assertRaisesRegex(ValidationError, "https://"):
                    SandboxDirectConnectionConfig(api_url="http://router", **options)

    def test_tls_options_accepted_on_https(self):
        config = SandboxDirectConnectionConfig(
            api_url="HTTPS://router",
            client_cert=["/c.crt", "/c.key"],
            ca_cert="/ca.pem",
        )
        self.assertEqual(config.client_cert, ("/c.crt", "/c.key"))
        self.assertEqual(config.ca_cert, "/ca.pem")

    def test_repr_hides_header_values(self):
        config = SandboxDirectConnectionConfig(
            api_url="https://router", extra_headers={"Authorization": "Bearer secret"}
        )
        self.assertNotIn("secret", repr(config))


class TestSandboxConnectorExtraHeadersAndTLS(unittest.TestCase):
    def _make_connector(self, config):
        connector = SandboxConnector(
            sandbox_id="my-sb",
            namespace="my-ns",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        mock_resp = MagicMock(spec=requests.Response)
        mock_resp.status_code = 200
        mock_resp.is_redirect = False
        mock_resp.raise_for_status.return_value = None
        connector.session = MagicMock()
        connector.session.request.return_value = mock_resp
        return connector

    def test_extra_headers_are_sent_with_routing_headers(self):
        config = SandboxDirectConnectionConfig(
            api_url="https://router", extra_headers={"Authorization": "Bearer t"}
        )
        connector = self._make_connector(config)

        connector.send_request("GET", "/execute")

        sent = connector.session.request.call_args.kwargs["headers"]
        self.assertEqual(sent["Authorization"], "Bearer t")
        self.assertEqual(sent["X-Sandbox-ID"], "my-sb")
        # Must not mutate the config.
        self.assertEqual(config.extra_headers, {"Authorization": "Bearer t"})

    def test_caller_headers_override_extra_headers(self):
        config = SandboxDirectConnectionConfig(
            api_url="https://router", extra_headers={"Authorization": "Bearer t"}
        )
        connector = self._make_connector(config)

        connector.send_request("GET", "/execute", headers={"authorization": "Bearer other"})

        sent = connector.session.request.call_args.kwargs["headers"]
        self.assertEqual(sent["authorization"], "Bearer other")
        self.assertNotIn("Authorization", sent)

    def test_explicit_none_headers_are_accepted(self):
        config = SandboxDirectConnectionConfig(
            api_url="https://router", extra_headers={"Authorization": "Bearer t"}
        )
        connector = self._make_connector(config)

        connector.send_request("GET", "/execute", headers=None)

        sent = connector.session.request.call_args.kwargs["headers"]
        self.assertEqual(sent["Authorization"], "Bearer t")

    def test_tls_options_are_passed_on_each_request(self):
        config = SandboxDirectConnectionConfig(
            api_url="https://router",
            client_cert=("/c.crt", "/c.key"),
            ca_cert="/ca.pem",
        )
        connector = self._make_connector(config)

        connector.send_request("GET", "/execute")

        kwargs = connector.session.request.call_args.kwargs
        self.assertEqual(kwargs["verify"], "/ca.pem")
        self.assertEqual(kwargs["cert"], ("/c.crt", "/c.key"))

    def test_no_tls_kwargs_without_tls_options(self):
        connector = self._make_connector(SandboxDirectConnectionConfig(api_url="https://router"))

        connector.send_request("GET", "/execute")

        kwargs = connector.session.request.call_args.kwargs
        self.assertNotIn("verify", kwargs)
        self.assertNotIn("cert", kwargs)

    def test_ca_cert_wins_over_requests_ca_bundle_env(self):
        # requests prefers REQUESTS_CA_BUNDLE over a session-level verify.
        config = SandboxDirectConnectionConfig(api_url="https://router", ca_cert="/ca.pem")
        connector = self._make_connector(config)
        connector.session = requests.Session()
        ok = MagicMock(spec=requests.Response)
        ok.status_code = 200
        ok.is_redirect = False
        connector.session.send = MagicMock(return_value=ok)

        with patch.dict(os.environ, {"REQUESTS_CA_BUNDLE": "/env-bundle.pem"}):
            connector.send_request("GET", "/execute")

        self.assertEqual(connector.session.send.call_args.kwargs["verify"], "/ca.pem")


class TestSandboxConnectorErrorHandling(unittest.TestCase):
    def _make_connector(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = SandboxConnector(
            sandbox_id="sb",
            namespace="ns",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        connector.strategy = DirectConnectionStrategy(config)
        connector.session = MagicMock()
        # Pretend a Pod IP was already resolved so a reset is detectable.
        connector._pod_ip = "10.0.0.5"
        connector._pod_ip_resolved = True
        return connector

    def _error_response(self, status_code):
        resp = MagicMock(spec=requests.Response)
        resp.status_code = status_code
        resp.is_redirect = False
        resp.raise_for_status.side_effect = requests.exceptions.HTTPError(response=resp)
        return resp

    def test_client_error_keeps_connection(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        connector = self._make_connector()
        connector.session.request.return_value = self._error_response(404)

        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("GET", "download/missing.txt")

        # A 404 means the sandbox answered: the connection must be left intact.
        self.assertEqual(ctx.exception.status_code, 404)
        self.assertEqual(connector._pod_ip, "10.0.0.5")
        self.assertTrue(connector._pod_ip_resolved)
        connector.session.close.assert_not_called()

    def test_streaming_client_error_closes_response(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        connector = self._make_connector()
        response = self._error_response(404)
        connector.session.request.return_value = response

        with self.assertRaises(SandboxRequestError):
            connector.send_request("GET", "download/missing.txt", stream=True)

        response.close.assert_called_once_with()
        connector.session.close.assert_not_called()

    def test_streaming_client_error_preserves_response_body(self):
        from k8s_agent_sandbox.connector import SandboxRequestError

        connector = self._make_connector()
        response = requests.Response()
        response.status_code = 404
        response.url = "http://sandbox/download/missing.txt"
        response.request = requests.Request("GET", response.url).prepare()
        response.raw = io.BytesIO(b"missing file")
        connector.session.request.return_value = response

        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("GET", "download/missing.txt", stream=True)

        self.assertEqual(ctx.exception.response.text, "missing file")

    def test_server_error_clears_pod_ip_but_keeps_tunnel(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        connector = self._make_connector()
        connector.session.request.return_value = self._error_response(503)

        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("GET", "run")

        # A 5xx often means the cached Pod IP went stale after a pod
        # replacement: drop it so the next request re-resolves, but the
        # tunnel carried a full response and must stay open.
        self.assertEqual(ctx.exception.status_code, 503)
        self.assertIsNone(connector._pod_ip)
        self.assertFalse(connector._pod_ip_resolved)
        connector.session.close.assert_not_called()

    def test_transport_failure_resets_connection(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        connector = self._make_connector()
        connector.session.request.side_effect = requests.exceptions.ConnectionError("refused")

        with self.assertRaises(SandboxRequestError):
            connector.send_request("GET", "download/x")

        # A genuine transport failure should reset the Pod IP and close the tunnel.
        self.assertIsNone(connector._pod_ip)
        self.assertFalse(connector._pod_ip_resolved)
        connector.session.close.assert_called()


class TestSandboxConnectorRetryExhaustion(unittest.TestCase):
    """A 5xx that exhausts urllib3's status retries must still reach the 5xx
    branch rather than surface as a responseless RetryError."""

    def _serve_503(self):
        class _H(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(503)
                self.end_headers()
                self.wfile.write(b"unavailable")

            def log_message(self, *args):
                pass

        server = HTTPServer(("127.0.0.1", 0), _H)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_address[1]}"

    def test_exhausted_retry_5xx_preserves_status_and_keeps_tunnel(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        connector = SandboxConnector(
            sandbox_id="sb",
            namespace="ns",
            connection_config=SandboxDirectConnectionConfig(api_url=self._serve_503()),
            k8s_helper=MagicMock(),
        )
        connector._pod_ip = "10.0.0.5"
        connector._pod_ip_resolved = True
        close_spy = MagicMock(wraps=connector.session.close)
        connector.session.close = close_spy

        # Patch sleep so urllib3's real backoff between the 5 retries is instant.
        with patch("time.sleep"):
            with self.assertRaises(SandboxRequestError) as ctx:
                connector.send_request("GET", "run")

        # raise_on_status=False lets the final 503 reach raise_for_status, so
        # the 5xx branch fires: status preserved, Pod IP dropped, tunnel kept.
        self.assertEqual(ctx.exception.status_code, 503)
        self.assertIsNone(connector._pod_ip)
        self.assertFalse(connector._pod_ip_resolved)
        close_spy.assert_not_called()

    def test_in_cluster_5xx_reresolves_pod_ip(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        api = self._serve_503()
        port = int(api.rsplit(":", 1)[1])
        ips = iter(["127.0.0.1", "10.0.0.99"])
        connector = SandboxConnector(
            sandbox_id="sb",
            namespace="ns",
            connection_config=SandboxInClusterConnectionConfig(server_port=port),
            k8s_helper=MagicMock(),
            get_pod_ip=lambda: next(ips),
        )

        with patch("time.sleep"):
            with self.assertRaises(SandboxRequestError) as ctx:
                connector.send_request("GET", "run")

        # In-cluster caches the Pod IP in the strategy's base URL; a 5xx must
        # invalidate it so the next connect() resolves the replacement Pod.
        self.assertEqual(ctx.exception.status_code, 503)
        self.assertFalse(connector.strategy._resolved)
        self.assertEqual(connector.connect(), f"http://10.0.0.99:{port}")


class TestLocalTunnelPreflightCheck(unittest.TestCase):
    """Unit tests for LocalTunnelConnectionStrategy._preflight_check_router_service."""

    def _make_strategy(self, router_namespace="agent-sandbox-system"):
        config = SandboxLocalTunnelConnectionConfig(router_namespace=router_namespace)
        return LocalTunnelConnectionStrategy(
            sandbox_id="my-sandbox", namespace="default", config=config
        )

    @patch("subprocess.run")
    def test_preflight_raises_with_namespace_when_service_not_found(self, mock_run):
        """SandboxPortForwardError must include the searched namespace when service is absent."""
        mock_run.return_value = MagicMock(
            returncode=1,
            stderr=b'Error from server (NotFound): services "sandbox-router-svc" not found',
        )
        strategy = self._make_strategy(router_namespace="my-custom-ns")

        with self.assertRaises(SandboxPortForwardError) as ctx:
            strategy._preflight_check_router_service()

        error_msg = str(ctx.exception)
        self.assertIn("my-custom-ns", error_msg)
        self.assertIn("router_namespace", error_msg)

    @patch("subprocess.run")
    def test_preflight_raises_with_router_namespace_hint(self, mock_run):
        """Error message must contain a hint to configure router_namespace."""
        mock_run.return_value = MagicMock(
            returncode=1,
            stderr=b'Error from server (NotFound): services "sandbox-router-svc" not found',
        )
        strategy = self._make_strategy()

        with self.assertRaises(SandboxPortForwardError) as ctx:
            strategy._preflight_check_router_service()

        self.assertIn("SandboxLocalTunnelConnectionConfig", str(ctx.exception))

    @patch("subprocess.run")
    def test_preflight_does_not_raise_on_transient_failure(self, mock_run):
        """Non-definitive kubectl failures (e.g. RBAC) must not block the tunnel."""
        mock_run.return_value = MagicMock(
            returncode=1,
            stderr=b"Error from server (Forbidden): services is forbidden",
        )
        strategy = self._make_strategy()
        # Should not raise
        strategy._preflight_check_router_service()

    @patch("subprocess.run")
    def test_preflight_does_not_raise_on_success(self, mock_run):
        """Successful kubectl get means service exists — no exception."""
        mock_run.return_value = MagicMock(returncode=0, stderr=b"")
        strategy = self._make_strategy()
        strategy._preflight_check_router_service()

    @patch("subprocess.run", side_effect=FileNotFoundError("kubectl not found"))
    def test_preflight_does_not_raise_when_kubectl_missing(self, _mock_run):
        """Missing kubectl binary must not block the tunnel attempt."""
        strategy = self._make_strategy()
        strategy._preflight_check_router_service()

    @patch("subprocess.run", side_effect=subprocess.TimeoutExpired(cmd="kubectl", timeout=10))
    def test_preflight_does_not_raise_on_timeout(self, _mock_run):
        """A slow API server timing out the preflight check must not block the tunnel."""
        strategy = self._make_strategy()
        strategy._preflight_check_router_service()

    @patch("subprocess.run")
    def test_preflight_latency_does_not_consume_port_forward_timeout(self, mock_run):
        """Preflight check duration must not eat into port_forward_ready_timeout.

        A slow preflight (simulated by advancing time before Popen starts) should
        leave the full port_forward_ready_timeout available for the readiness loop.
        """
        # Preflight passes.
        mock_run.return_value = MagicMock(returncode=0, stderr=b"")
        # Use a tight timeout so any budget erosion would cause a real timeout.
        config = SandboxLocalTunnelConnectionConfig(
            router_namespace="ns", port_forward_ready_timeout=1
        )
        strategy = LocalTunnelConnectionStrategy(
            sandbox_id="sb", namespace="default", config=config
        )

        mock_proc = MagicMock()
        # Process is alive (poll returns None) and port opens on first check.
        mock_proc.poll.return_value = None

        time_calls = iter([
            0.0,    # start_time (metrics)
            8.0,    # port_forward_start (after simulated 8 s preflight)
            8.1,    # first monotonic() in while condition — well within 1 s budget
            8.2,    # finally-block monotonic() for end-to-end latency measurement
        ])

        with patch("k8s_agent_sandbox.connector.time.monotonic", side_effect=time_calls), \
             patch("subprocess.Popen", return_value=mock_proc), \
             patch.object(strategy, "_get_free_port", return_value=19877), \
             patch.object(strategy, "_is_port_open", return_value=True):
            url = strategy.connect()

        self.assertEqual(url, "http://127.0.0.1:19877")

    @patch("subprocess.run")
    def test_connect_error_message_includes_namespace(self, mock_run):
        """When port-forward crashes, the error message must include router_namespace."""
        # Pre-flight passes, then the Popen process crashes immediately.
        mock_run.return_value = MagicMock(returncode=0, stderr=b"")
        strategy = self._make_strategy(router_namespace="custom-ns")

        mock_proc = MagicMock()
        # poll() returns non-None → process already exited
        mock_proc.poll.return_value = 1
        mock_proc.communicate.return_value = (
            b"",
            b'error: services "sandbox-router-svc" not found',
        )

        with patch("subprocess.Popen", return_value=mock_proc):
            with patch.object(strategy, "_get_free_port", return_value=19876):
                with self.assertRaises(SandboxPortForwardError) as ctx:
                    strategy.connect()

        self.assertIn("custom-ns", str(ctx.exception))
        self.assertIn("router_namespace", str(ctx.exception))

    @patch("subprocess.run")
    def test_connect_omits_namespace_hint_on_unrelated_crash(self, mock_run):
        """A crash unrelated to a missing service must not blame router_namespace."""
        mock_run.return_value = MagicMock(returncode=0, stderr=b"")
        strategy = self._make_strategy(router_namespace="custom-ns")

        mock_proc = MagicMock()
        mock_proc.poll.return_value = 1
        mock_proc.communicate.return_value = (
            b"",
            b"error: unable to listen on port 19876: address already in use",
        )

        with patch("subprocess.Popen", return_value=mock_proc):
            with patch.object(strategy, "_get_free_port", return_value=19876):
                with self.assertRaises(SandboxPortForwardError) as ctx:
                    strategy.connect()

        message = str(ctx.exception)
        self.assertNotIn("router_namespace", message)
        self.assertIn("address already in use", message)


class TestTunnelConcurrency(unittest.TestCase):
    """Concurrent use of one Sandbox must not leak port-forward processes."""

    def _slow_popen(self, *args, **kwargs):
        # Widen the check-then-spawn window so concurrent connects overlap.
        time.sleep(0.05)
        process = MagicMock()
        process.poll.return_value = None
        return process

    def _connect_concurrently(self, strategy):
        barrier = threading.Barrier(8)

        def worker():
            barrier.wait()
            return strategy.connect()

        with ThreadPoolExecutor(max_workers=8) as pool:
            futures = [pool.submit(worker) for _ in range(8)]
            for future in futures:
                future.result()

    @patch.object(LocalTunnelConnectionStrategy, "_preflight_check_router_service")
    @patch.object(LocalTunnelConnectionStrategy, "_is_port_open", return_value=True)
    @patch("subprocess.Popen")
    def test_local_tunnel_concurrent_connect_spawns_one_process(self, mock_popen, _, _preflight):
        mock_popen.side_effect = self._slow_popen
        strategy = LocalTunnelConnectionStrategy(
            sandbox_id="sb", namespace="ns",
            config=SandboxLocalTunnelConnectionConfig(),
        )

        self._connect_concurrently(strategy)

        self.assertEqual(mock_popen.call_count, 1)

    @patch.object(SandboxdPodTunnelStrategy, "_is_port_open", return_value=True)
    @patch("subprocess.Popen")
    def test_sandboxd_tunnel_concurrent_connect_spawns_one_process(self, mock_popen, _):
        mock_popen.side_effect = self._slow_popen
        strategy = SandboxdPodTunnelStrategy(
            sandbox_id="sb", namespace="ns",
            config=SandboxdPodTunnelConnectionConfig(),
            get_pod_name=lambda: "sb-pod",
        )

        self._connect_concurrently(strategy)

        self.assertEqual(mock_popen.call_count, 1)


class TestTunnelTargetsInjectedApiClient(unittest.TestCase):
    """kubectl must reach the cluster an injected ApiClient targets, not the ambient one."""

    def _api_client(self):
        cfg = k8s_client.Configuration()
        cfg.host = "https://cluster-b:6443"
        cfg.api_key = {"authorization": "secret"}
        cfg.api_key_prefix = {"authorization": "Bearer"}
        return k8s_client.ApiClient(configuration=cfg)

    def _recording_popen(self, calls):
        def popen(cmd, **kwargs):
            # kubectl reads the kubeconfig at startup, so capture it here.
            path = cmd[cmd.index("--kubeconfig") + 1] if "--kubeconfig" in cmd else None
            content = None
            if path:
                with open(path) as f:
                    content = json.load(f)
            calls.append((cmd, path, content))
            process = MagicMock()
            process.poll.return_value = None
            return process
        return popen

    def _assert_targets_cluster_b(self, calls):
        cmd, path, content = calls[0]
        self.assertEqual(content["clusters"][0]["cluster"]["server"], "https://cluster-b:6443")
        self.assertEqual(content["users"][0]["user"]["token"], "secret")
        self.assertFalse(os.path.exists(path), "kubeconfig must not outlive the connect")
        return cmd

    @patch.object(LocalTunnelConnectionStrategy, "_is_port_open", return_value=True)
    @patch("subprocess.run", return_value=MagicMock(returncode=0, stderr=b""))
    @patch("subprocess.Popen")
    def test_local_tunnel_preflight_and_port_forward_use_injected_cluster(
        self, mock_popen, mock_run, _
    ):
        popen_calls = []
        mock_popen.side_effect = self._recording_popen(popen_calls)
        strategy = LocalTunnelConnectionStrategy(
            "sb", "ns", SandboxLocalTunnelConnectionConfig(), self._api_client()
        )

        strategy.connect()

        cmd = self._assert_targets_cluster_b(popen_calls)
        self.assertEqual(cmd[:3], ["kubectl", "port-forward", "svc/sandbox-router-svc"])
        preflight = mock_run.call_args.args[0]
        self.assertEqual(preflight[:3], ["kubectl", "get", "svc/sandbox-router-svc"])
        self.assertEqual(
            preflight[preflight.index("--kubeconfig") + 1],
            cmd[cmd.index("--kubeconfig") + 1],
            "preflight and port-forward must share one kubeconfig",
        )

    @patch.object(LocalTunnelConnectionStrategy, "_get_free_port", return_value=18080)
    @patch.object(LocalTunnelConnectionStrategy, "_is_port_open", return_value=True)
    @patch("subprocess.run", return_value=MagicMock(returncode=0, stderr=b""))
    @patch("subprocess.Popen")
    def test_local_tunnel_without_api_client_adds_no_flags(self, mock_popen, mock_run, *_):
        calls = []
        mock_popen.side_effect = self._recording_popen(calls)
        strategy = LocalTunnelConnectionStrategy("sb", "ns", SandboxLocalTunnelConnectionConfig())

        strategy.connect()

        self.assertEqual(
            calls[0][0],
            ["kubectl", "port-forward", "svc/sandbox-router-svc", "18080:8080",
             "-n", "agent-sandbox-system"],
        )
        self.assertEqual(
            mock_run.call_args.args[0],
            ["kubectl", "get", "svc/sandbox-router-svc", "-n", "agent-sandbox-system"],
        )

    @patch.object(LocalTunnelConnectionStrategy, "_preflight_check_router_service")
    @patch("subprocess.Popen")
    def test_local_tunnel_removes_kubeconfig_when_port_forward_crashes(self, mock_popen, _):
        process = MagicMock()
        process.poll.return_value = 1
        process.communicate.return_value = (b"", b"boom")
        calls = []

        def popen(cmd, **kwargs):
            calls.append(cmd[cmd.index("--kubeconfig") + 1])
            return process

        mock_popen.side_effect = popen
        strategy = LocalTunnelConnectionStrategy(
            "sb", "ns", SandboxLocalTunnelConnectionConfig(), self._api_client()
        )

        with self.assertRaises(SandboxPortForwardError):
            strategy.connect()

        self.assertFalse(os.path.exists(calls[0]))

    @patch.object(SandboxdPodTunnelStrategy, "_is_port_open", return_value=True)
    @patch("subprocess.Popen")
    def test_sandboxd_pod_tunnel_uses_injected_cluster(self, mock_popen, _):
        calls = []
        mock_popen.side_effect = self._recording_popen(calls)
        strategy = SandboxdPodTunnelStrategy(
            "sb", "ns", SandboxdPodTunnelConnectionConfig(),
            get_pod_name=lambda: "sb-pod", api_client=self._api_client(),
        )

        strategy.connect()

        cmd = self._assert_targets_cluster_b(calls)
        self.assertEqual(cmd[:3], ["kubectl", "port-forward", "pod/sb-pod"])

    @patch.object(SandboxdPodTunnelStrategy, "_is_port_open", return_value=True)
    @patch("subprocess.Popen")
    def test_reused_tunnel_does_not_write_another_kubeconfig(self, mock_popen, _):
        calls = []
        mock_popen.side_effect = self._recording_popen(calls)
        strategy = SandboxdPodTunnelStrategy(
            "sb", "ns", SandboxdPodTunnelConnectionConfig(),
            get_pod_name=lambda: "sb-pod", api_client=self._api_client(),
        )

        strategy.connect()
        strategy.connect()

        self.assertEqual(len(calls), 1)

    def test_connector_passes_the_helpers_injected_client_to_tunnels(self):
        injected = self._api_client()
        helper = MagicMock(injected_api_client=injected)

        for config, expected in (
            (SandboxLocalTunnelConnectionConfig(), LocalTunnelConnectionStrategy),
            (SandboxdPodTunnelConnectionConfig(), SandboxdPodTunnelStrategy),
        ):
            connector = SandboxConnector(
                sandbox_id="sb", namespace="ns", connection_config=config, k8s_helper=helper,
            )
            self.assertIsInstance(connector.strategy, expected)
            self.assertIs(connector.strategy._api_client, injected)


class TestSandboxConnectorTransportRetry(unittest.TestCase):
    """A transport failure (no HTTP response) must be retried by the urllib3
    adapter for idempotent methods, not surfaced after a single attempt."""

    def _serve_drop(self):
        attempts = self.attempts = []

        class _H(BaseHTTPRequestHandler):
            def do_GET(self):
                attempts.append(1)
                self.connection.close()  # drop before any response

            def log_message(self, *args):
                pass

        server = HTTPServer(("127.0.0.1", 0), _H)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_address[1]}"

    def test_transport_failure_is_retried_for_idempotent_methods(self):
        from k8s_agent_sandbox.connector import SandboxRequestError
        connector = SandboxConnector(
            sandbox_id="sb",
            namespace="ns",
            connection_config=SandboxDirectConnectionConfig(api_url=self._serve_drop()),
            k8s_helper=MagicMock(),
        )

        with patch("time.sleep"):
            with self.assertRaises(SandboxRequestError):
                connector.send_request("GET", "run")

        self.assertGreater(len(self.attempts), 1)


if __name__ == "__main__":
    unittest.main()
