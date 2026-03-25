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
from k8s_agent_sandbox.constants import POD_NAME_ANNOTATION


class TestSandboxClient(unittest.TestCase):

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def setUp(self, MockK8sHelper):
        self.client = SandboxClient()
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class

    @patch('os.urandom')
    def test_create_sandbox_success(self, mock_urandom):
        mock_urandom.return_value = b'\x12\x34\xab\xcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {"annotations": {POD_NAME_ANNOTATION: "custom-pod-name"}}
        }
        
        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance
        
        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready') as mock_wait:
            
            sandbox = self.client.create_sandbox("test-template", "test-namespace")
            
            mock_create_claim.assert_called_once_with("sandbox-claim-1234abcd", "test-template", "test-namespace")
            self.mock_k8s_helper.resolve_sandbox_name.assert_called_once_with("sandbox-claim-1234abcd", "test-namespace", 180)
            mock_wait.assert_called_once_with("resolved-id", "test-namespace", 180)
            self.assertEqual(sandbox, mock_sandbox_instance)
            
            # Verify the new sandbox is tracked in the registry
            self.assertEqual(len(self.client._active_connection_sandboxes), 1)
            self.assertEqual(self.client._active_connection_sandboxes[("test-namespace", "sandbox-claim-1234abcd")], mock_sandbox_instance)

    @patch('os.urandom')
    def test_create_sandbox_failure_cleanup(self, mock_urandom):
        mock_urandom.return_value = b'\x12\x34\xab\xcd'
        self.mock_k8s_helper.resolve_sandbox_name.side_effect = Exception("Timeout Error")
        
        with patch.object(self.client, '_create_claim') as mock_create_claim:
            with self.assertRaises(Exception) as context:
                self.client.create_sandbox("test-template", "test-namespace")
                
            self.assertEqual(str(context.exception), "Timeout Error")
            # Ensure delete_sandbox_claim is called to cleanup orphan claim on failure
            self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with("sandbox-claim-1234abcd", "test-namespace")

    def test_get_sandbox_existing_active(self):
        mock_sandbox = MagicMock()
        mock_sandbox.is_active = True
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_sandbox
        
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}

        sandbox = self.client.get_sandbox("test-claim", "test-namespace")
        
        self.assertEqual(sandbox, mock_sandbox)
        self.mock_sandbox_class.assert_not_called()

        mock_inactive_sandbox = MagicMock()
        mock_inactive_sandbox.is_active = False
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_inactive_sandbox
        
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}
        
        mock_new_sandbox = MagicMock()
        self.mock_sandbox_class.return_value = mock_new_sandbox
        
        sandbox = self.client.get_sandbox("test-claim", "test-namespace")
        
        self.assertEqual(sandbox, mock_new_sandbox)
        self.assertEqual(self.client._active_connection_sandboxes[("test-namespace", "test-claim")], mock_new_sandbox)
        self.mock_k8s_helper.get_sandbox.assert_called_with("resolved-id", "test-namespace")
        self.mock_sandbox_class.assert_called_once()

    def test_get_sandbox_not_found_k8s_error(self):
        self.mock_k8s_helper.resolve_sandbox_name.side_effect = Exception("Not found")
        
        with self.assertRaises(RuntimeError) as context:
            self.client.get_sandbox("test-claim", "test-namespace")
            
        self.assertIn("Sandbox claim 'test-claim' not found or resolution failed in namespace 'test-namespace'", str(context.exception))

    def test_get_sandbox_underlying_sandbox_not_found(self):
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = None
        
        with self.assertRaises(RuntimeError) as context:
            self.client.get_sandbox("test-claim", "test-namespace")
            
        self.assertIn("Sandbox claim 'test-claim' not found or resolution failed in namespace 'test-namespace'", str(context.exception))
        self.assertIn("Underlying Sandbox 'resolved-id' not found.", str(context.exception))

    def test_list_active_sandboxes(self):
        mock_active = MagicMock()
        mock_active.is_active = True
        self.client._active_connection_sandboxes[("ns1", "active-claim")] = mock_active
        
        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        self.client._active_connection_sandboxes[("ns2", "inactive-claim")] = mock_inactive
        
        active_list = self.client.list_active_sandboxes()
        
        self.assertEqual(active_list, [("ns1", "active-claim")])
        self.assertNotIn(("ns2", "inactive-claim"), self.client._active_connection_sandboxes)

    def test_list_all_sandboxes(self):
        self.mock_k8s_helper.list_sandbox_claims.return_value = ["sandbox-1", "sandbox-2"]
        
        result = self.client.list_all_sandboxes("test-namespace")
        
        self.mock_k8s_helper.list_sandbox_claims.assert_called_once_with("test-namespace")
        self.assertEqual(result, ["sandbox-1", "sandbox-2"])

    def test_delete_sandbox_in_registry(self):
        mock_sandbox = MagicMock()
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_sandbox
        
        self.client.delete_sandbox("test-claim", "test-namespace")
        
        mock_sandbox.terminate.assert_called_once()
        self.assertNotIn(("test-namespace", "test-claim"), self.client._active_connection_sandboxes)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_delete_sandbox_not_in_registry_with_fallback(self):
        with patch.object(self.client, 'get_sandbox', side_effect=Exception("Not found")):
            self.client.delete_sandbox("test-claim", "test-namespace")
            
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with("test-claim", "test-namespace")
        
    def test_delete_sandbox_not_in_registry_success(self):
        mock_retrieved_sandbox = MagicMock()
        with patch.object(self.client, 'get_sandbox', return_value=mock_retrieved_sandbox):
            self.client.delete_sandbox("test-claim", "test-namespace")
            
        mock_retrieved_sandbox.terminate.assert_called_once()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_delete_all(self):
        mock_sandbox1 = MagicMock()
        mock_sandbox1.namespace = "ns1"
        self.client._active_connection_sandboxes[("ns1", "claim1")] = mock_sandbox1
        
        mock_sandbox2 = MagicMock()
        mock_sandbox2.namespace = "ns2"
        self.client._active_connection_sandboxes[("ns2", "claim2")] = mock_sandbox2
        
        with patch.object(self.client, 'delete_sandbox') as mock_delete:
            self.client.delete_all()
            self.assertEqual(mock_delete.call_count, 2)
            mock_delete.assert_any_call("claim1", namespace="ns1")
            mock_delete.assert_any_call("claim2", namespace="ns2")

    def test_get_sandbox_pod_name_with_annotation(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {
                "annotations": {
                    POD_NAME_ANNOTATION: "annotated-pod-name"
                }
            }
        }
        pod_name = self.client._get_sandbox_pod_name("sandbox-id", "test-namespace")
        self.assertEqual(pod_name, "annotated-pod-name")

    def test_get_sandbox_pod_name_without_annotation(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {}
        }
        pod_name = self.client._get_sandbox_pod_name("sandbox-id", "test-namespace")
        self.assertEqual(pod_name, "sandbox-id")

    def test_get_sandbox_pod_name_no_sandbox(self):
        self.mock_k8s_helper.get_sandbox.return_value = None
        pod_name = self.client._get_sandbox_pod_name("sandbox-id", "test-namespace")
        self.assertEqual(pod_name, "sandbox-id")

    def test_create_claim(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"
        
        self.client._create_claim("test-claim", "test-template", "test-namespace")
        
        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-template", "test-namespace", 
            {"opentelemetry.io/trace-context": "trace-data"}
        )

    def test_wait_for_sandbox_ready(self):
        self.client._wait_for_sandbox_ready("sandbox-id", "test-namespace", 45)
        
        self.mock_k8s_helper.wait_for_sandbox_ready.assert_called_once_with(
            "sandbox-id", "test-namespace", 45
        )


if __name__ == '__main__':
    unittest.main()
