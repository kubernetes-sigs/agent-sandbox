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

import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import MagicMock, patch

import requests

from k8s_agent_sandbox.connector import (
    DirectConnectionStrategy,
    GatewayConnectionStrategy,
    LocalTunnelConnectionStrategy,
    InClusterConnectionStrategy,
    SandboxConnector,
)
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

    def test_post_requests_are_not_retried(self):
        connector = self._make_connector(
            SandboxDirectConnectionConfig(api_url="http://router")
        )
        retry_policy = connector.session.get_adapter("http://").max_retries

        self.assertEqual(
            set(retry_policy.allowed_methods), {"GET", "PUT", "DELETE"}
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


if __name__ == "__main__":
    unittest.main()
