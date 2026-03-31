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
from http import HTTPStatus
from http.server import HTTPServer, BaseHTTPRequestHandler
from unittest.mock import patch, MagicMock

from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry

from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig
from k8s_agent_sandbox.exceptions import (
    SandboxPortForwardError,
    SandboxRequestError,
)


class SandboxHandler(BaseHTTPRequestHandler):
    """Minimal api handler with basic routing to exercise error paths

    Routes:
        POST /run-500 -> 500 Internal Server Error
        POST /run -> 202 Accepted (happy path)
        POST /run-busy -> 409 Conflict
        POST /run-shutdown -> 503 Service Unavailable
        GET  /health -> 200 OK
    """

    def do_POST(self):
        if self.path == "/run":
            self._respond(
                HTTPStatus.ACCEPTED,
                {"status": "accepted", "message": "Trajectory execution started"},
            )
        elif self.path == "/run-busy":
            self._respond(
                HTTPStatus.CONFLICT,
                {"detail": "A task is already running. Each sandbox can only execute one task at a time."},
            )
        elif self.path == "/run-shutdown":
            self._respond(
                HTTPStatus.SERVICE_UNAVAILABLE,
                {"detail": "Service is shutting down, cannot accept new jobs"},
            )
        elif self.path == "/run-500":
            self._respond(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                {"detail": "Internal server error"},
            )
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "Not found"})

    def do_GET(self):
        if self.path == "/health":
            self._respond(
                HTTPStatus.OK,
                {"status": "healthy", "message": "Sandbox service is running"},
            )
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
        # Disable retries so errors surface immediately
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
        config = SandboxDirectConnectionConfig(
            api_url="http://192.0.2.0",
            server_port=8888,
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

        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run", timeout=1)

        self.assertIsNone(ctx.exception.status_code)

    def test_port_forward_crash_raises_sandbox_port_forward_error(self):
        """Validates SandboxPortForwardError when verify_connection detects a crash."""
        connector = self._make_connector()

        with patch.object(
            connector.strategy, "verify_connection",
            side_effect=SandboxPortForwardError("Kubectl Port-Forward crashed!"),
        ):
            with self.assertRaises(SandboxPortForwardError):
                connector.send_request("GET", "health")

    def test_port_forward_error_is_catchable_as_runtime_error(self):
        """Backwards compatibility: SandboxPortForwardError is still a RuntimeError."""
        connector = self._make_connector()

        with patch.object(
            connector.strategy, "verify_connection",
            side_effect=SandboxPortForwardError("Kubectl Port-Forward crashed!"),
        ):
            with self.assertRaises(RuntimeError):
                connector.send_request("GET", "health")


if __name__ == "__main__":
    unittest.main()
