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
        assert "X-Sandbox-ID header is required" in resp.json()["detail"]

    def test_invalid_namespace_format(self, client):
        resp = client.post(
            "/execute",
            headers={
                "X-Sandbox-ID": "my-sandbox",
                "X-Sandbox-Namespace": "bad namespace!",
            },
        )
        assert resp.status_code == 400
        assert "Invalid namespace format." == resp.json()["detail"]

    def test_invalid_port_format(self, client):
        resp = client.post(
            "/execute",
            headers={
                "X-Sandbox-ID": "my-sandbox",
                "X-Sandbox-Port": "not-a-number",
            },
        )
        assert resp.status_code == 400
        assert "Invalid port format." == resp.json()["detail"]

    def test_valid_namespace_with_hyphens(self, client):
        """Namespaces like 'my-ns' should pass validation and result in a connection attempt."""
        with patch.object(
            sandbox_router.client,
            "send",
            new_callable=AsyncMock,
            side_effect=httpx.ConnectError("stop here")
        ):
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Namespace": "my-namespace",
                },
            )
        # Expect 502 because the send is mocked to raise ConnectError
        assert resp.status_code == 502
        assert "Could not connect to the backend sandbox" in resp.json()["detail"]


class TestProxyTimeout:
    def test_default_timeout(self):
        assert sandbox_router.DEFAULT_PROXY_TIMEOUT == 180.0

    def test_env_var_overrides_timeout(self):
        with patch.dict(os.environ, {"PROXY_TIMEOUT_SECONDS": "600"}):
            importlib.reload(sandbox_router)
            assert sandbox_router.proxy_timeout == 600.0
            assert sandbox_router.client.timeout.connect == 600.0
            assert sandbox_router.client.timeout.read == 600.0

        importlib.reload(sandbox_router)

    def test_default_when_env_var_unset(self):
        with patch.dict(os.environ, {}, clear=True):
            importlib.reload(sandbox_router)
            assert sandbox_router.proxy_timeout == 180.0

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
            request_obj = built_request[0][0]
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

    @pytest.mark.asyncio
    @patch.object(httpx.AsyncClient, "send", new_callable=AsyncMock)
    async def test_request_body_streamed(self, mock_send, client):
        """Verify that the request body is passed as a stream to httpx."""
        mock_resp = AsyncMock(spec=httpx.Response)
        mock_resp.status_code = 200
        mock_resp.headers = {}
        async def _async_iter(items):
            for item in items:
                yield item
        mock_resp.aiter_bytes.return_value = _async_iter([b"OK"])
        mock_send.return_value = mock_resp

        # Correctly create a larger payload
        test_content = b'{"key": "value", "padding": "' + b"x" * 2048 + b'"}'
        assert len(test_content) > 2048

        with TestClient(sandbox_router.app) as test_client:
            test_client.post(
                "/execute",
                headers={"X-Sandbox-ID": "test-sandbox"},
                content=test_content,
            )

        mock_send.assert_called_once()
        args, kwargs = mock_send.call_args
        sent_request = args[0]

        assert hasattr(
            sent_request.stream, "__aiter__"
        ), "Content should be an async iterable"
