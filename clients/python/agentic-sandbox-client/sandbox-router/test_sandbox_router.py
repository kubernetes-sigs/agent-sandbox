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

import importlib
import os
from unittest.mock import AsyncMock, patch

import httpx
import pytest
from fastapi.testclient import TestClient

import sandbox_router


@pytest.fixture
def client():
    return TestClient(sandbox_router.app)


class TestHealthCheck:
    def test_returns_ok(self, client):
        resp = client.get("/healthz")
        assert resp.status_code == 200
        assert resp.json() == {"status": "ok"}


class TestProxyRequestValidation:
    def test_missing_sandbox_id_header(self, client):
        resp = client.post("/execute")
        assert resp.status_code == 400
        assert "X-Sandbox-ID" in resp.json()["detail"]

    def test_invalid_namespace_format(self, client):
        resp = client.post(
            "/execute",
            headers={
                "X-Sandbox-ID": "my-sandbox",
                "X-Sandbox-Namespace": "bad namespace!",
            },
        )
        assert resp.status_code == 400
        assert "Invalid namespace" in resp.json()["detail"]

    def test_invalid_port_format(self, client):
        resp = client.post(
            "/execute",
            headers={
                "X-Sandbox-ID": "my-sandbox",
                "X-Sandbox-Port": "not-a-number",
            },
        )
        assert resp.status_code == 400
        assert "Invalid port" in resp.json()["detail"]

    def test_valid_namespace_with_hyphens(self, client):
        """Namespaces like 'my-ns' should pass validation (connection will fail, not 400)."""
        resp = client.post(
            "/execute",
            headers={
                "X-Sandbox-ID": "my-sandbox",
                "X-Sandbox-Namespace": "my-namespace",
            },
        )
        # Should NOT be a 400 validation error; it will fail at connection level
        assert resp.status_code != 400


class TestProxyTimeout:
    def test_default_timeout(self):
        assert sandbox_router.DEFAULT_PROXY_TIMEOUT == 180.0

    def test_env_var_overrides_timeout(self):
        with patch.dict(os.environ, {"PROXY_TIMEOUT_SECONDS": "600"}):
            importlib.reload(sandbox_router)
            assert sandbox_router.proxy_timeout == 600.0
            assert sandbox_router.client.timeout.connect == 600.0
            assert sandbox_router.client.timeout.read == 600.0

        # Restore default
        importlib.reload(sandbox_router)
    def test_default_when_env_var_unset(self):
        with patch.dict(os.environ, {}, clear=True):
            importlib.reload(sandbox_router)
            assert sandbox_router.proxy_timeout == 180.0

        # Restore default
        importlib.reload(sandbox_router)


class TestProxyRouting:
    def test_connect_error_returns_502(self, client):
        """When the target sandbox is unreachable, the router should return 502."""
        with patch.object(
            sandbox_router.client,
            "send",
            new_callable=AsyncMock,
            side_effect=httpx.ConnectError("Connection refused"),
        ):
            resp = client.post(
                "/execute",
                headers={"X-Sandbox-ID": "unreachable-sandbox"},
                content=b'{"command": "echo hello"}',
            )
            assert resp.status_code == 502
            assert "unreachable-sandbox" in resp.json()["detail"]

    def test_target_url_construction(self, client):
        """Verify the router builds the correct internal DNS URL."""
        with patch.object(
            sandbox_router.client,
            "send",
            new_callable=AsyncMock,
            side_effect=httpx.ConnectError("expected"),
        ) as mock_send:
            client.post(
                "/some/path",
                headers={
                    "X-Sandbox-ID": "test-box",
                    "X-Sandbox-Namespace": "prod",
                    "X-Sandbox-Port": "9999",
                },
            )
            built_request = mock_send.call_args
            request_obj = built_request[0][0]  # first positional arg
            assert "test-box.prod.svc.cluster.local:9999/some/path" in str(
                request_obj.url
            )

    def test_original_host_header_not_forwarded(self, client):
        """The original 'host' header should not be forwarded to the sandbox."""
        captured_request = {}

        async def capture_send(req, **kwargs):
            captured_request["headers"] = dict(req.headers)
            raise httpx.ConnectError("stop here")

        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "host": "evil.example.com",
                },
            )
            forwarded_host = captured_request.get("headers", {}).get("host", "")
            assert "evil.example.com" not in forwarded_host

    def test_query_parameters_forwarded(self, client):
        """Query parameters should be preserved in the proxied request."""
        captured_request = {}

        async def capture_send(req, **kwargs):
            captured_request["params"] = req.url.params
            raise httpx.ConnectError("stop here")

        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            client.get(
                "/execute?cmd=ls&arg=-la",
                headers={"X-Sandbox-ID": "my-sandbox"},
            )
            assert captured_request.get("params", {}).get("cmd") == "ls"
            assert captured_request.get("params", {}).get("arg") == "-la"
