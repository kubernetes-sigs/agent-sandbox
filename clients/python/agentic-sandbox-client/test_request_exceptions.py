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

"""
Integration tests for _request() exception handling.

Spins up a real HTTP server that mimics sandbox responses (409, 503, 202, etc.)
and exercises the full SandboxClient._request() path over real HTTP — no mocks.
"""

import json
import threading
import unittest
from http import HTTPStatus
from http.server import HTTPServer, BaseHTTPRequestHandler
from unittest.mock import patch

from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from kubernetes import config

from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.exceptions import (
    SandboxNotReadyError,
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
    """Integration tests: real HTTP server + real SandboxClient._request()."""

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
        cls.server_thread.join(timeout=5)

    def _make_sandbox(self) -> SandboxClient:
        """Creates a barebones SandboxClient while mocking the k8s config."""
        with patch("kubernetes.config.load_incluster_config", side_effect=config.ConfigException("not in cluster")), \
            patch("kubernetes.config.load_kube_config"):
            sandbox = SandboxClient(
                template_name="test-template",
                api_url=f"http://127.0.0.1:{self.port}",
                server_port=self.port,
            )
        # set by __enter__ / _create_claim; but is mocked.
        sandbox.claim_name = "test-claim"
        
        # we need immediate failures without counting retries
        adapter = HTTPAdapter(max_retries=Retry(total=0))
        sandbox.session.mount("http://", adapter)

        return sandbox

    def test_run_accepted(self):
        """POST /run returns 202."""
        sandbox = self._make_sandbox()
        response = sandbox._request("POST", "run", json={"query": "test"})
        self.assertEqual(response.status_code, 202)
        self.assertEqual(response.json()["status"], "accepted")

    def test_health_ok(self):
        """GET /health returns 200."""
        sandbox = self._make_sandbox()
        response = sandbox._request("GET", "health")
        self.assertEqual(response.status_code, 200)

    def test_409_raises_sandbox_request_error(self):
        """Validates 409 SandboxRequestError"""
        sandbox = self._make_sandbox()

        with self.assertRaises(SandboxRequestError) as ctx:
            sandbox._request("POST", "run-busy")

        self.assertEqual(ctx.exception.status_code, 409)
        body = ctx.exception.response.json()
        self.assertIn("already running", body["detail"])

    def test_409_is_catchable_as_runtime_error(self):
        """Backwards compatability check to verify this isn't a breaking change, checking the SandboxRequestError is still a RuntimeError."""
        sandbox = self._make_sandbox()

        with self.assertRaises(RuntimeError):
            sandbox._request("POST", "run-busy")

    def test_503_raises_sandbox_request_error(self):
        """Validates 503 SandboxRequestError"""
        sandbox = self._make_sandbox()

        with self.assertRaises(SandboxRequestError) as ctx:
            sandbox._request("POST", "run-shutdown")

        self.assertEqual(ctx.exception.status_code, 503)
        body = ctx.exception.response.json()
        self.assertIn("shutting down", body["detail"])

    def test_500_raises_sandbox_request_error(self):
        """Validates 500 SandboxRequestError"""
        sandbox = self._make_sandbox()

        with self.assertRaises(SandboxRequestError) as ctx:
            sandbox._request("POST", "run-500")

        self.assertEqual(ctx.exception.status_code, 500)

    def test_connection_refused_has_no_status_code(self):
        """Validates no status_code does not raise an unhandled exception."""
        sandbox = self._make_sandbox()
        sandbox.base_url = "http://127.0.0.1:1"

        with self.assertRaises(SandboxRequestError) as ctx:
            sandbox._request("POST", "run")

        self.assertIsNone(ctx.exception.status_code)

    def test_not_ready_raises_sandbox_not_ready_error(self):
        """Validates SandboxNotReadyError is raised when base_url is None."""
        sandbox = self._make_sandbox()
        sandbox.base_url = None

        with self.assertRaises(SandboxNotReadyError):
            sandbox._request("GET", "health")


if __name__ == "__main__":
    unittest.main()
