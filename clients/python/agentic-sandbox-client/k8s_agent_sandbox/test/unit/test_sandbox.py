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

from k8s_agent_sandbox.sandbox import Sandbox
from k8s_agent_sandbox.models import SandboxLocalTunnelConnectionConfig, SandboxTracerConfig


class TestSandbox(unittest.TestCase):

    @patch('k8s_agent_sandbox.sandbox.Filesystem')
    @patch('k8s_agent_sandbox.sandbox.CommandExecutor')
    @patch('k8s_agent_sandbox.sandbox.create_tracer_manager')
    @patch('k8s_agent_sandbox.sandbox.SandboxConnector')
    @patch('k8s_agent_sandbox.sandbox.K8sHelper')
    def setUp(self, mock_k8s_helper, mock_connector, mock_create_tracer_manager, mock_command_executor, mock_filesystem):
        self.mock_k8s_helper_cls = mock_k8s_helper
        self.mock_connector_cls = mock_connector
        self.mock_create_tracer_manager_func = mock_create_tracer_manager
        self.mock_command_executor_cls = mock_command_executor
        self.mock_filesystem_cls = mock_filesystem

        self.mock_k8s_helper = mock_k8s_helper.return_value
        self.mock_connector = mock_connector.return_value
        self.mock_tracer_manager = MagicMock()
        self.mock_tracer = MagicMock()
        mock_create_tracer_manager.return_value = (self.mock_tracer_manager, self.mock_tracer)
        self.mock_command_executor = mock_command_executor.return_value
        self.mock_filesystem = mock_filesystem.return_value

        self.sandbox_id = "test-sandbox"
        self.namespace = "test-namespace"
        self.claim_name = "test-claim"

        self.sandbox = Sandbox(
            claim_name=self.claim_name,
            sandbox_id=self.sandbox_id,
            namespace=self.namespace,
        )

    def test_init_with_defaults(self):
        """Tests sandbox initialization with default configurations."""
        self.mock_k8s_helper_cls.assert_called_once()

        self.mock_connector_cls.assert_called_once()
        args, kwargs = self.mock_connector_cls.call_args
        self.assertEqual(kwargs['sandbox_id'], self.sandbox_id)
        self.assertEqual(kwargs['namespace'], self.namespace)
        self.assertIsInstance(kwargs['connection_config'], SandboxLocalTunnelConnectionConfig)
        self.assertEqual(kwargs['k8s_helper'], self.mock_k8s_helper)

        self.mock_create_tracer_manager_func.assert_called_once()
        self.assertIsInstance(self.mock_create_tracer_manager_func.call_args[0][0], SandboxTracerConfig)

        self.mock_command_executor_cls.assert_called_once_with(self.mock_connector, self.mock_tracer, 'sandbox-client')
        self.mock_filesystem_cls.assert_called_once_with(self.mock_connector, self.mock_tracer, 'sandbox-client')

        self.assertEqual(self.sandbox.claim_name, self.claim_name)
        self.assertEqual(self.sandbox.sandbox_id, self.sandbox_id)
        self.assertEqual(self.sandbox.namespace, self.namespace)
        self.assertFalse(self.sandbox._is_closed)

    @patch('k8s_agent_sandbox.sandbox.Filesystem')
    @patch('k8s_agent_sandbox.sandbox.CommandExecutor')
    @patch('k8s_agent_sandbox.sandbox.create_tracer_manager')
    @patch('k8s_agent_sandbox.sandbox.SandboxConnector')
    @patch('k8s_agent_sandbox.sandbox.K8sHelper')
    def test_init_with_custom_args(self, mock_k8s_helper, mock_connector, mock_create_tracer_manager, mock_command_executor, mock_filesystem):
        """Tests sandbox initialization with custom arguments."""
        mock_k8s_helper_instance = MagicMock()
        mock_connection_config = MagicMock()
        mock_tracer_config = SandboxTracerConfig(trace_service_name="custom-tracer")
        mock_tracer, mock_manager = MagicMock(), MagicMock()
        mock_create_tracer_manager.return_value = (mock_manager, mock_tracer)

        sandbox = Sandbox(
            sandbox_id="custom-id",
            namespace="custom-ns",
            claim_name="custom-claim",
            connection_config=mock_connection_config,
            tracer_config=mock_tracer_config,
            k8s_helper=mock_k8s_helper_instance
        )

        mock_k8s_helper.assert_not_called()
        self.assertEqual(sandbox.k8s_helper, mock_k8s_helper_instance)

        mock_connector.assert_called_once_with(
            sandbox_id="custom-id",
            namespace="custom-ns",
            connection_config=mock_connection_config,
            k8s_helper=mock_k8s_helper_instance
        )

        mock_create_tracer_manager.assert_called_once_with(mock_tracer_config)
        mock_command_executor.assert_called_once_with(mock_connector.return_value, mock_tracer, "custom-tracer")
        mock_filesystem.assert_called_once_with(mock_connector.return_value, mock_tracer, "custom-tracer")

    def test_get_pod_name_with_annotation(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {
                "annotations": {
                    'agents.x-k8s.io/pod-name': "annotated-pod-name"
                }
            }
        }
        self.assertEqual(self.sandbox.get_pod_name(), "annotated-pod-name")

    def test_get_pod_name_fallback(self):
        self.mock_k8s_helper.get_sandbox.return_value = None
        self.assertEqual(self.sandbox.get_pod_name(), self.sandbox_id)

    def test_status_not_found(self):
        self.mock_k8s_helper.get_sandbox.return_value = None
        status, message = self.sandbox.status()
        
        self.assertEqual(status, "SandboxNotFound")
        self.assertEqual(message, "Sandbox object not found in Kubernetes.")
        self.mock_k8s_helper.get_sandbox.assert_called_once_with(self.sandbox_id, self.namespace)

    def test_status_ready(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "conditions": [
                    {"type": "Ready", "status": "True", "message": ""}
                ]
            }
        }
        status, message = self.sandbox.status()
        
        self.assertEqual(status, "SandboxReady")
        self.assertEqual(message, "")

    def test_status_not_ready_with_message(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "conditions": [
                    {"type": "Ready", "status": "False", "message": "Pod is initializing"}
                ]
            }
        }
        status, message = self.sandbox.status()
        
        self.assertEqual(status, "SandboxNotReady")
        self.assertEqual(message, "Pod is initializing")

    def test_status_no_ready_condition(self):
        self.mock_k8s_helper.get_sandbox.return_value = {
            "status": {
                "conditions": [
                    {"type": "PodScheduled", "status": "True"}
                ]
            }
        }
        status, message = self.sandbox.status()
        
        self.assertEqual(status, "SandboxNotReady")
        self.assertEqual(message, "Unknown message")

    def test_properties(self):
        """Tests the commands and files properties."""
        self.assertEqual(self.sandbox.commands, self.mock_command_executor)
        self.assertEqual(self.sandbox.files, self.mock_filesystem)

    def test_is_active(self):
        """Tests the is_active property."""
        self.assertTrue(self.sandbox.is_active)
        self.sandbox._is_closed = True
        self.assertFalse(self.sandbox.is_active)

    def test_close_connection(self):
        """Tests the internal _close_connection method."""
        self.sandbox._close_connection()

        self.mock_connector.close.assert_called_once()
        self.assertIsNone(self.sandbox.commands)
        self.assertIsNone(self.sandbox.files)
        self.mock_tracer_manager.end_lifecycle_span.assert_called_once()
        self.assertTrue(self.sandbox._is_closed)

        # Test idempotency
        self.mock_connector.close.reset_mock()
        self.sandbox._close_connection()
        self.mock_connector.close.assert_not_called()

    @patch('logging.error')
    def test_close_connection_with_tracing_error(self, mock_logging_error):
        """Tests _close_connection with an error in tracing."""
        self.mock_tracer_manager.end_lifecycle_span.side_effect = Exception("Tracer error")
        self.sandbox._close_connection()

        self.mock_connector.close.assert_called_once()
        self.assertTrue(self.sandbox._is_closed)
        mock_logging_error.assert_called_once_with("Failed to end tracing span: Tracer error")

    def test_terminate(self):
        """Tests the terminate method."""
        with patch.object(self.sandbox, '_close_connection') as mock_close:
            self.sandbox.terminate()
            mock_close.assert_called_once()

        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(self.claim_name, self.namespace)

if __name__ == '__main__':
    unittest.main()
