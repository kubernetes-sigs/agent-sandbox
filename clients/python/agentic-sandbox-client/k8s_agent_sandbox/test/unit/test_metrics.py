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
from unittest.mock import MagicMock, patch
import time
import socket
import subprocess
import prometheus_client
from k8s_agent_sandbox.connector import GatewayConnectionStrategy, LocalTunnelConnectionStrategy, DirectConnectionStrategy
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig, SandboxLocalTunnelConnectionConfig, SandboxDirectConnectionConfig
from k8s_agent_sandbox.metrics import sandbox_client_discovery_latency_ms

class TestMetrics(unittest.TestCase):
    def setUp(self):
        self.registry = prometheus_client.REGISTRY

    @patch('k8s_agent_sandbox.k8s_helper.K8sHelper')
    def test_gateway_connection_latency_metric(self, mock_k8s_helper):
        config = SandboxGatewayConnectionConfig(gateway_name="test", gateway_namespace="default")
        helper = mock_k8s_helper()
        helper.wait_for_gateway_ip.return_value = "1.2.3.4"
        
        strategy = GatewayConnectionStrategy(config, helper)
        
        before_count = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'gateway', 'status': 'success'}) or 0.0
        
        url = strategy.connect()
        
        after_count = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'gateway', 'status': 'success'}) or 0.0
        
        self.assertEqual(url, "http://1.2.3.4")
        self.assertEqual(after_count, before_count + 1.0)

    @patch('k8s_agent_sandbox.k8s_helper.K8sHelper')
    def test_gateway_connection_latency_metric_failure(self, mock_k8s_helper):
        config = SandboxGatewayConnectionConfig(gateway_name="test", gateway_namespace="default")
        helper = mock_k8s_helper()
        helper.wait_for_gateway_ip.side_effect = Exception("failed to wait for gateway IP")
        
        strategy = GatewayConnectionStrategy(config, helper)
        
        before_count = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'gateway', 'status': 'failure'}) or 0.0
        
        with self.assertRaises(Exception):
            strategy.connect()
            
        after_count = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'gateway', 'status': 'failure'}) or 0.0
        
        self.assertEqual(after_count, before_count + 1.0)

    @patch('subprocess.Popen')
    @patch('socket.create_connection')
    @patch('socket.socket')
    def test_port_forward_latency_metric(self, mock_socket_cls, mock_create_conn, mock_popen):
        config = SandboxLocalTunnelConnectionConfig()
        
        process = MagicMock()
        process.poll.return_value = None 
        mock_popen.return_value = process
        
        s = MagicMock()
        s.getsockname.return_value = ('127.0.0.1', 12345)
        mock_socket_cls.return_value.__enter__.return_value = s
        
        mock_create_conn.return_value = MagicMock()
        
        strategy = LocalTunnelConnectionStrategy(sandbox_id="test", namespace="default", config=config)
        
        before_count = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'port_forward', 'status': 'success'}) or 0.0
        
        url = strategy.connect()
        
        after_count = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'port_forward', 'status': 'success'}) or 0.0
        
        self.assertEqual(url, "http://127.0.0.1:12345")
        self.assertEqual(after_count, before_count + 1.0)

    def test_direct_connection_no_metric(self):
        config = SandboxDirectConnectionConfig(api_url="http://preconfigured.com")
        strategy = DirectConnectionStrategy(config)
        
        before_count_port_forward = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'port_forward', 'status': 'success'}) or 0.0
        before_count_gateway = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'gateway', 'status': 'success'}) or 0.0
        
        url = strategy.connect()
        
        after_count_port_forward = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'port_forward', 'status': 'success'}) or 0.0
        after_count_gateway = self.registry.get_sample_value('sandbox_client_discovery_latency_ms_count', labels={'mode': 'gateway', 'status': 'success'}) or 0.0
        
        self.assertEqual(url, "http://preconfigured.com")
        self.assertEqual(after_count_port_forward, before_count_port_forward)
        self.assertEqual(after_count_gateway, before_count_gateway)

if __name__ == '__main__':
    unittest.main()
