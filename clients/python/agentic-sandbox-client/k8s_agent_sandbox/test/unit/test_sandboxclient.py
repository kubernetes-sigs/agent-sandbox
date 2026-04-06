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

import json
import threading
import unittest
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import HTTPServer, BaseHTTPRequestHandler
from unittest.mock import MagicMock, patch, ANY

from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry

from kubernetes import config as k8s_config
from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig
from k8s_agent_sandbox.constants import POD_NAME_ANNOTATION
from k8s_agent_sandbox.exceptions import (
    SandboxPortForwardError,
    SandboxRequestError,
)
from k8s_agent_sandbox.k8s_helper import K8sHelper


class TestSandboxClient(unittest.TestCase):

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def setUp(self, MockK8sHelper):
        self.client = SandboxClient()
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class

    @patch('uuid.uuid4')
    def test_create_sandbox_success(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {
            "metadata": {"annotations": {POD_NAME_ANNOTATION: "custom-pod-name"}}
        }
        
        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance
        
        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready') as mock_wait:
            
            sandbox = self.client.create_sandbox("test-template", "test-namespace")
            
            mock_create_claim.assert_called_once_with("sandbox-claim-1234abcd", "test-template", "test-namespace", labels=None, lifecycle=None)
            self.mock_k8s_helper.resolve_sandbox_name.assert_called_once_with("sandbox-claim-1234abcd", "test-namespace", 180)
            mock_wait.assert_called_once_with("resolved-id", "test-namespace", ANY)
            self.assertEqual(sandbox, mock_sandbox_instance)
            
            # Verify the new sandbox is tracked in the registry
            self.assertEqual(len(self.client._active_connection_sandboxes), 1)
            self.assertEqual(self.client._active_connection_sandboxes[("test-namespace", "sandbox-claim-1234abcd")], mock_sandbox_instance)

    @patch('uuid.uuid4')
    def test_create_sandbox_failure_cleanup(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
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

    def test_delete_sandbox_not_in_registry_success(self):
        with patch.object(self.client, '_delete_claim') as mock_delete_claim:
            self.client.delete_sandbox("test-claim", "test-namespace")
            
        mock_delete_claim.assert_called_once_with("test-claim", "test-namespace")

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

    @patch('uuid.uuid4')
    def test_create_sandbox_with_labels(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        labels = {"agent": "code-agent", "team": "platform"}

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox("test-template", "test-namespace", labels=labels)

            mock_create_claim.assert_called_once_with(
                "sandbox-claim-1234abcd", "test-template", "test-namespace",
                labels={"agent": "code-agent", "team": "platform"},
                lifecycle=None,
            )

    def test_create_claim_with_labels(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"

        labels = {"agent": "code-agent"}
        self.client._create_claim("test-claim", "test-template", "test-namespace", labels=labels)

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-template", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels={"agent": "code-agent"},
            lifecycle=None,
        )

    def test_create_claim(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"
        
        self.client._create_claim("test-claim", "test-template", "test-namespace")
        
        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-template", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels=None,
            lifecycle=None,
        )

    def test_validate_labels_rejects_invalid_value(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"agent": "invalid value!"})
        self.assertIn("invalid characters", str(ctx.exception))

    def test_validate_labels_rejects_too_long_value(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"agent": "a" * 64})
        self.assertIn("exceeds max length", str(ctx.exception))

    def test_validate_labels_rejects_empty_key(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"": "value"})
        self.assertIn("Label key cannot be empty", str(ctx.exception))

    def test_validate_labels_rejects_invalid_key(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"bad key!": "value"})
        self.assertIn("invalid characters", str(ctx.exception))

    def test_validate_labels_rejects_too_long_key(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"a" * 64: "value"})
        self.assertIn("exceeds max length", str(ctx.exception))

    def test_validate_labels_accepts_prefixed_key(self):
        SandboxClient._validate_labels({"app.kubernetes.io/name": "my-app"})

    def test_validate_labels_rejects_invalid_prefix(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"BAD PREFIX/name": "value"})
        self.assertIn("valid DNS subdomain", str(ctx.exception))

    def test_validate_labels_accepts_single_char_prefix(self):
        SandboxClient._validate_labels({"a/name": "value"})

    def test_validate_labels_accepts_empty_value(self):
        SandboxClient._validate_labels({"key": ""})

    def test_validate_labels_accepts_valid(self):
        SandboxClient._validate_labels({"agent": "code-agent", "team": "platform-123"})

    def test_wait_for_sandbox_ready(self):
        self.client._wait_for_sandbox_ready("sandbox-id", "test-namespace", 45)
        
        self.mock_k8s_helper.wait_for_sandbox_ready.assert_called_once_with(
            "sandbox-id", "test-namespace", 45
        )

    @patch('uuid.uuid4')
    def test_create_sandbox_with_shutdown_after_seconds(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox(
                "test-template", "test-namespace", shutdown_after_seconds=300
            )

            mock_create_claim.assert_called_once()
            call_kwargs = mock_create_claim.call_args
            lifecycle = call_kwargs[1].get("lifecycle") or call_kwargs[0][3] if len(call_kwargs[0]) > 3 else call_kwargs[1].get("lifecycle")
            self.assertIsNotNone(lifecycle)
            self.assertEqual(lifecycle["shutdownPolicy"], "Delete")
            self.assertIn("shutdownTime", lifecycle)

    @patch('uuid.uuid4')
    def test_create_sandbox_without_shutdown_after_seconds(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox("test-template", "test-namespace")

            call_kwargs = mock_create_claim.call_args
            lifecycle = call_kwargs[1].get("lifecycle")
            self.assertIsNone(lifecycle)

    @patch("k8s_agent_sandbox.lifecycle.datetime")
    def test_create_claim_with_lifecycle(self, mock_datetime):
        frozen_now = datetime(2026, 6, 15, 12, 0, 0, tzinfo=timezone.utc)
        mock_datetime.now.return_value = frozen_now
        mock_datetime.side_effect = lambda *a, **kw: datetime(*a, **kw)

        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = None

        lifecycle = {
            "shutdownTime": "2026-06-15T12:05:00Z",
            "shutdownPolicy": "Delete",
        }
        self.client._create_claim(
            "test-claim", "test-template", "test-namespace", lifecycle=lifecycle
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-template", "test-namespace",
            annotations={},
            labels=None,
            lifecycle=lifecycle,
        )

    def test_create_claim_without_lifecycle(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = None

        self.client._create_claim("test-claim", "test-template", "test-namespace")

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-template", "test-namespace",
            annotations={},
            labels=None,
            lifecycle=None,
        )

    def test_shutdown_after_seconds_validation_zero(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", shutdown_after_seconds=0)
        self.assertIn("positive", str(ctx.exception))

    def test_shutdown_after_seconds_validation_negative(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", shutdown_after_seconds=-1)
        self.assertIn("positive", str(ctx.exception))

    def test_shutdown_after_seconds_validation_bool(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", shutdown_after_seconds=True)
        self.assertIn("integer", str(ctx.exception))

    def test_shutdown_after_seconds_validation_float(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", shutdown_after_seconds=1.5)
        self.assertIn("integer", str(ctx.exception))

    def test_shutdown_after_seconds_validation_string(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", shutdown_after_seconds="10")
        self.assertIn("integer", str(ctx.exception))


class SandboxHandler(BaseHTTPRequestHandler):
    """Minimal api handler with basic routing to exercise error paths."""

    def do_POST(self):
        if self.path == "/run":
            self._respond(HTTPStatus.ACCEPTED, {"status": "accepted", "message": "Trajectory execution started"})
        elif self.path == "/run-busy":
            self._respond(HTTPStatus.CONFLICT, {"detail": "A task is already running. Each sandbox can only execute one task at a time."})
        elif self.path == "/run-shutdown":
            self._respond(HTTPStatus.SERVICE_UNAVAILABLE, {"detail": "Service is shutting down, cannot accept new jobs"})
        elif self.path == "/run-500":
            self._respond(HTTPStatus.INTERNAL_SERVER_ERROR, {"detail": "Internal server error"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "Not found"})

    def do_GET(self):
        if self.path == "/health":
            self._respond(HTTPStatus.OK, {"status": "healthy", "message": "Sandbox service is running"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "Not found"})

    def _respond(self, status: HTTPStatus, body: dict):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        payload = json.dumps(body).encode()
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


class TestRequestExceptions(unittest.TestCase):
    """Integration tests: real HTTP server + SandboxConnector.send_request()."""

    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), SandboxHandler)
        cls.port = cls.server.server_address[1]
        cls.server_thread = threading.Thread(target=cls.server.serve_forever)
        cls.server_thread.daemon = True
        cls.server_thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.server_thread.join(timeout=5)

    def _make_connector(self) -> SandboxConnector:
        """Creates a SandboxConnector pointing at the local test server."""
        config = SandboxDirectConnectionConfig(
            api_url=f"http://127.0.0.1:{self.port}",
            server_port=self.port,
        )
        k8s_helper = MagicMock()
        connector = SandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=k8s_helper,
        )
        adapter = HTTPAdapter(max_retries=Retry(total=0))
        connector.session.mount("http://", adapter)
        return connector

    def test_run_accepted(self):
        """POST /run returns 202."""
        connector = self._make_connector()
        response = connector.send_request("POST", "run", json={"query": "test"})
        self.assertEqual(response.status_code, 202)
        self.assertEqual(response.json()["status"], "accepted")

    def test_health_ok(self):
        """GET /health returns 200."""
        connector = self._make_connector()
        response = connector.send_request("GET", "health")
        self.assertEqual(response.status_code, 200)

    def test_409_raises_sandbox_request_error(self):
        """Validates 409 SandboxRequestError."""
        connector = self._make_connector()
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run-busy")
        self.assertEqual(ctx.exception.status_code, 409)
        body = ctx.exception.response.json()
        self.assertIn("already running", body["detail"])

    def test_409_is_catchable_as_runtime_error(self):
        """Backwards compatibility: SandboxRequestError is still a RuntimeError."""
        connector = self._make_connector()
        with self.assertRaises(RuntimeError):
            connector.send_request("POST", "run-busy")

    def test_503_raises_sandbox_request_error(self):
        """Validates 503 SandboxRequestError."""
        connector = self._make_connector()
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run-shutdown")
        self.assertEqual(ctx.exception.status_code, 503)
        body = ctx.exception.response.json()
        self.assertIn("shutting down", body["detail"])

    def test_500_raises_sandbox_request_error(self):
        """Validates 500 SandboxRequestError."""
        connector = self._make_connector()
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run-500")
        self.assertEqual(ctx.exception.status_code, 500)

    def test_connection_refused_has_no_status_code(self):
        """Validates no status_code when server is unreachable."""
        config = SandboxDirectConnectionConfig(api_url="http://192.0.2.0", server_port=8888)
        k8s_helper = MagicMock()
        connector = SandboxConnector(
            sandbox_id="test-sandbox", namespace="default",
            connection_config=config, k8s_helper=k8s_helper,
        )
        adapter = HTTPAdapter(max_retries=Retry(total=0))
        connector.session.mount("http://", adapter)
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run", timeout=1)
        self.assertIsNone(ctx.exception.status_code)

    def test_port_forward_crash_raises_sandbox_port_forward_error(self):
        """Validates SandboxPortForwardError when verify_connection detects a crash."""
        connector = self._make_connector()
        with patch.object(connector.strategy, "verify_connection",
                          side_effect=SandboxPortForwardError("Kubectl Port-Forward crashed!")):
            with self.assertRaises(SandboxPortForwardError):
                connector.send_request("GET", "health")

    def test_port_forward_error_is_catchable_as_runtime_error(self):
        """Backwards compatibility: SandboxPortForwardError is still a RuntimeError."""
        connector = self._make_connector()
        with patch.object(connector.strategy, "verify_connection",
                          side_effect=SandboxPortForwardError("Kubectl Port-Forward crashed!")):
            with self.assertRaises(RuntimeError):
                connector.send_request("GET", "health")


class TestK8sHelperWatchNoneEvents(unittest.TestCase):
    """Tests that watch streams gracefully handle None events.

    The `watch` api can yield `None` when the underlying
    connection times out/drops/etc. These tests verify that the watch
    loop can handle this gracefully.
    """

    def setUp(self):
        with patch("kubernetes.config.load_incluster_config", side_effect=k8s_config.ConfigException("not in cluster")), \
             patch("kubernetes.config.load_kube_config"):
            self.helper = K8sHelper()
        self.helper.custom_objects_api = MagicMock()

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_skips_none_events(self, mock_watch_cls):
        """None events from the watch stream should be skipped, not crash."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-sandbox"},
                "status": {
                    "conditions": [
                        {"type": "Ready", "status": "True"},
                    ],
                },
            },
        }
        mock_watch.stream.return_value = [None, None, ready_event]
        self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_all_none_times_out(self, mock_watch_cls):
        """A stream of only None events should exhaust the watch and raise TimeoutError."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        mock_watch.stream.return_value = [None, None, None]

        with self.assertRaises(TimeoutError):
            self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_skips_none_events(self, mock_watch_cls):
        """None events in the gateway watch should be skipped."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        gateway_ready_event = {
            "type": "MODIFIED",
            "object": {
                "status": {
                    "addresses": [{"value": "10.0.0.1"}],
                },
            },
        }
        mock_watch.stream.return_value = [None, gateway_ready_event]

        ip = self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=10)
        self.assertEqual(ip, "10.0.0.1")


if __name__ == '__main__':
    unittest.main()
