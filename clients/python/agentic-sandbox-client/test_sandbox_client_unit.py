# Copyright 2025 The Kubernetes Authors.
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
from unittest.mock import MagicMock, patch, ANY
import sys
import os

from agentic_sandbox.sandbox_client import SandboxClient, SandboxStatus

class TestSandboxClient(unittest.TestCase):

    def setUp(self):
        # Patch Kubernetes config loading to avoid errors
        self.patcher_load_kube_config = patch('kubernetes.config.load_kube_config')
        self.patcher_load_incluster_config = patch('kubernetes.config.load_incluster_config')
        self.mock_load_kube_config = self.patcher_load_kube_config.start()
        self.mock_load_incluster_config = self.patcher_load_incluster_config.start()

        # Patch Kubernetes Client classes
        self.patcher_custom_objects = patch('kubernetes.client.CustomObjectsApi')
        self.mock_custom_objects_cls = self.patcher_custom_objects.start()
        self.mock_custom_objects_api = self.mock_custom_objects_cls.return_value

        self.patcher_core_v1 = patch('kubernetes.client.CoreV1Api')
        self.mock_core_v1_cls = self.patcher_core_v1.start()
        self.mock_core_v1_api = self.mock_core_v1_cls.return_value

    def tearDown(self):
        self.patcher_load_kube_config.stop()
        self.patcher_load_incluster_config.stop()
        self.patcher_custom_objects.stop()
        self.patcher_core_v1.stop()

    def test_initialization(self):
        """Test that the client initializes with correct defaults."""
        client = SandboxClient(template_name="test-template")
        self.assertEqual(client.template_name, "test-template")
        self.assertEqual(client.namespace, "default")
        self.assertIsNone(client.base_url)

    def test_create_claim(self):
        """Test that _create_claim calls the Kubernetes API correctly."""
        client = SandboxClient(template_name="test-template")
        client._create_claim()

        self.assertIsNotNone(client.claim_name)
        self.assertTrue(client.claim_name.startswith("sandbox-claim-"))

        self.mock_custom_objects_api.create_namespaced_custom_object.assert_called_once()
        call_args = self.mock_custom_objects_api.create_namespaced_custom_object.call_args
        self.assertEqual(call_args.kwargs['group'], "extensions.agents.x-k8s.io")
        self.assertEqual(call_args.kwargs['version'], "v1alpha1")
        self.assertEqual(call_args.kwargs['plural'], "sandboxclaims")
        self.assertEqual(call_args.kwargs['namespace'], "default")
        self.assertEqual(call_args.kwargs['body']['spec']['sandboxTemplateRef']['name'], "test-template")

    @patch('kubernetes.watch.Watch')
    def test_wait_for_sandbox_ready(self, mock_watch_cls):
        """Test waiting for the sandbox to become ready."""
        mock_watch = mock_watch_cls.return_value
        
        # Simulate a watch event where the sandbox is Ready
        mock_event = {
            'type': 'MODIFIED',
            'object': {
                'metadata': {
                    'name': 'test-sandbox',
                    'annotations': {'agents.x-k8s.io/pod-name': 'test-pod-123'}
                },
                'status': {
                    'conditions': [
                        {'type': 'Ready', 'status': 'True'}
                    ]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event]

        client = SandboxClient(template_name="test-template")
        client.claim_name = "test-claim"
        
        client._wait_for_sandbox_ready()

        self.assertEqual(client.sandbox_name, "test-sandbox")
        self.assertEqual(client.pod_name, "test-pod-123")

    def test_status_running(self):
        """Test fetching status when pod is running."""
        client = SandboxClient(template_name="test-template")
        client.claim_name = "test-claim"
        client.pod_name = "test-claim"
        
        # Mock the pod status response
        mock_pod = MagicMock()
        mock_pod.status.phase = "Running"
        self.mock_core_v1_api.read_namespaced_pod.return_value = mock_pod

        status = client.status()
        
        self.assertEqual(status, SandboxStatus.RUNNING)
        self.mock_core_v1_api.read_namespaced_pod.assert_called_with(
            name="test-claim", namespace="default"
        )

    @patch('requests.Session')
    def test_run_command_success(self, mock_session_cls):
        """Test running a command successfully."""
        mock_session = mock_session_cls.return_value
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {
            'stdout': 'Hello World',
            'stderr': '',
            'exit_code': 0
        }
        mock_session.request.return_value = mock_response

        client = SandboxClient(template_name="test-template", api_url="http://localhost:8080")
        result = client.run("echo Hello World")

        self.assertEqual(result.stdout, "Hello World")
        self.assertEqual(result.exit_code, 0)

if __name__ == '__main__':
    unittest.main()
