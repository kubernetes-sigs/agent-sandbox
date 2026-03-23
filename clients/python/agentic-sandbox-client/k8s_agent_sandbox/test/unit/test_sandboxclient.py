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

import os
import unittest
from unittest.mock import MagicMock, patch, ANY

from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.sandbox import Sandbox


class TestSandboxClient(unittest.TestCase):

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    @patch('k8s_agent_sandbox.sandbox_client.Sandbox')
    def test_create_sandbox_success(self, MockSandbox, MockK8sHelper):
        client = SandboxClient()
        client.sandbox_class = MockSandbox
        client._create_claim = MagicMock()
        client._wait_for_sandbox_ready = MagicMock()
        
        mock_sandbox_instance = MagicMock()
        MockSandbox.return_value = mock_sandbox_instance
        
        sandbox = client.create_sandbox("test-template", "test-namespace")
        
        self.assertEqual(sandbox, mock_sandbox_instance)
        self.assertTrue(client._create_claim.called)
        self.assertTrue(client._wait_for_sandbox_ready.called)
        
        # Verify the new sandbox is tracked in the registry
        self.assertEqual(len(client._active_connection_sandboxes), 1)
        self.assertEqual(list(client._active_connection_sandboxes.values())[0], mock_sandbox_instance)

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_create_sandbox_failure_cleanup(self, MockK8sHelper):
        client = SandboxClient()
        client._create_claim = MagicMock()
        client._wait_for_sandbox_ready = MagicMock(side_effect=Exception("Timeout Error"))
        
        with self.assertRaises(Exception) as context:
            client.create_sandbox("test-template", "test-namespace")
            
        self.assertEqual(str(context.exception), "Timeout Error")
        # Ensure delete_sandbox_claim is called to cleanup orphan claim on failure
        client.k8s_helper.delete_sandbox_claim.assert_called_once_with(ANY, "test-namespace")
        
    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    @patch('k8s_agent_sandbox.sandbox_client.Sandbox')
    def test_get_sandbox_existing_active(self, MockSandbox, MockK8sHelper):
        client = SandboxClient()
        client.sandbox_class = MockSandbox
        mock_sandbox = MagicMock()
        mock_sandbox.is_active = True
        client._active_connection_sandboxes["test-id"] = mock_sandbox
        
        sandbox = client.get_sandbox("test-id", "test-namespace")
        
        self.assertEqual(sandbox, mock_sandbox)
        MockSandbox.assert_not_called()

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    @patch('k8s_agent_sandbox.sandbox_client.Sandbox')
    def test_get_sandbox_inactive_recreates(self, MockSandbox, MockK8sHelper):
        client = SandboxClient()
        client.sandbox_class = MockSandbox
        
        # Setup inactive sandbox in registry
        mock_inactive_sandbox = MagicMock()
        mock_inactive_sandbox.is_active = False
        client._active_connection_sandboxes["test-id"] = mock_inactive_sandbox
        
        # Mock K8s helper to confirm the sandbox resource still exists in K8s
        client.k8s_helper.get_sandbox.return_value = {"metadata": {}}
        
        # Mock the newly created sandbox handle
        mock_new_sandbox = MagicMock()
        MockSandbox.return_value = mock_new_sandbox
        
        sandbox = client.get_sandbox("test-id", "test-namespace")
        
        self.assertEqual(sandbox, mock_new_sandbox)
        self.assertEqual(client._active_connection_sandboxes["test-id"], mock_new_sandbox)
        client.k8s_helper.get_sandbox.assert_called_once_with("test-id", "test-namespace")
        MockSandbox.assert_called_once()

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_get_sandbox_not_found(self, MockK8sHelper):
        client = SandboxClient()
        client.k8s_helper.get_sandbox.return_value = None
        
        with self.assertRaises(RuntimeError) as context:
            client.get_sandbox("test-id", "test-namespace")
            
        self.assertIn("Sandbox 'test-id' not found in namespace 'test-namespace'", str(context.exception))

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_list_active_sandboxes(self, MockK8sHelper):
        client = SandboxClient()
        
        mock_active = MagicMock()
        mock_active.is_active = True
        client._active_connection_sandboxes["active-id"] = mock_active
        
        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        client._active_connection_sandboxes["inactive-id"] = mock_inactive
        
        active_list = client.list_active_sandboxes()
        
        self.assertEqual(active_list, ["active-id"])
        # Ensure inactive sandbox is lazily cleaned up from the registry
        self.assertNotIn("inactive-id", client._active_connection_sandboxes)

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_list_all_sandboxes(self, MockK8sHelper):
        client = SandboxClient()
        client.k8s_helper.list_sandboxes.return_value = ["sandbox-1", "sandbox-2"]
        
        result = client.list_all_sandboxes("test-namespace")
        
        client.k8s_helper.list_sandboxes.assert_called_once_with("test-namespace")
        self.assertEqual(result, ["sandbox-1", "sandbox-2"])

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_delete_all(self, MockK8sHelper):
        client = SandboxClient()
        
        mock_sandbox1 = MagicMock()
        mock_sandbox1.namespace = "ns1"
        client._active_connection_sandboxes["id1"] = mock_sandbox1
        
        mock_sandbox2 = MagicMock()
        mock_sandbox2.namespace = "ns2"
        client._active_connection_sandboxes["id2"] = mock_sandbox2
        
        with patch.object(client, 'delete_sandbox') as mock_delete:
            client.delete_all()
            self.assertEqual(mock_delete.call_count, 2)
            mock_delete.assert_any_call("id1", namespace="ns1")
            mock_delete.assert_any_call("id2", namespace="ns2")

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_create_claim(self, MockK8sHelper):
        client = SandboxClient()
        client.tracing_manager = MagicMock()
        client.tracing_manager.get_trace_context_json.return_value = "trace-data"
        
        client._create_claim("test-claim", "test-template", "test-namespace")
        
        client.k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-template", "test-namespace", 
            {"opentelemetry.io/trace-context": "trace-data"}
        )

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_wait_for_sandbox_ready(self, MockK8sHelper):
        client = SandboxClient(sandbox_ready_timeout=45)
        client._wait_for_sandbox_ready("test-claim", "test-namespace")
        
        client.k8s_helper.wait_for_sandbox_ready.assert_called_once_with(
            "test-claim", "test-namespace", 45
        )


if __name__ == '__main__':
    unittest.main()
