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

import subprocess
import unittest
from unittest.mock import MagicMock, patch

import requests

from k8s_agent_sandbox.connector import (
    DirectConnectionStrategy,
    GatewayConnectionStrategy,
    LocalTunnelConnectionStrategy,
    InClusterConnectionStrategy,
    SandboxConnector,
)
from k8s_agent_sandbox.exceptions import SandboxPortForwardError
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
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


class TestSandboxConnectorStrategySelection(unittest.TestCase):
    def _make_connector(self, config):
        return SandboxConnector(
            sandbox_id="sb",
            namespace="ns",
            connection_config=config,
            k8s_helper=MagicMock(),
        )

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


if __name__ == "__main__":
    unittest.main()
