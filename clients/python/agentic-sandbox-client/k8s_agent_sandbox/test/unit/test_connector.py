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
from unittest.mock import MagicMock

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
        with self.assertRaises((ValueError, Exception)):
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

    def test_in_cluster_url_is_pod_dns(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        strategy = InClusterConnectionStrategy("my-sb", "my-ns", config)
        connector, mock_session = self._make_connector_with_strategy(strategy, config)
        mock_session.request.return_value = self._mock_ok_response()

        connector.send_request("POST", "execute")

        call_args, call_kwargs = mock_session.request.call_args
        url = call_args[1]
        self.assertEqual(url, "http://my-sb.my-ns.svc.cluster.local:8888/execute")


if __name__ == "__main__":
    unittest.main()
