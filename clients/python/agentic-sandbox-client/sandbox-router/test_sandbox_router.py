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

import asyncio
import importlib
import os
import queue
from unittest.mock import AsyncMock, patch

import httpx
import pytest
from fastapi.testclient import TestClient
from starlette.datastructures import Headers, URL

os.environ["ALLOW_UNAUTHENTICATED_ROUTER"] = "true"
import sandbox_router


@pytest.fixture
def client():
    return TestClient(sandbox_router.app)


@pytest.fixture(autouse=True)
def reload_router():
    # Save the original environment before the test
    orig_env = dict(os.environ)
    yield
    # Restore original environment variables
    os.environ.clear()
    os.environ.update(orig_env)
    # Reload the module under the original environment to restore clean baseline
    importlib.reload(sandbox_router)


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

    def test_invalid_port_bounds(self, client):
        for bad_port in ["0", "65536", "-80", "100000"]:
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Port": bad_port,
                },
            )
            assert resp.status_code == 400
            assert "Invalid port format." == resp.json()["detail"]

    def test_invalid_sandbox_id_format(self, client):
        resp = client.post(
            "/execute",
            headers={
                "X-Sandbox-ID": "bad.sandbox.id",
            },
        )
        assert resp.status_code == 400
        assert "Invalid sandbox ID format." == resp.json()["detail"]

    def test_invalid_pod_ip_address_verification(self, client):
        # Invalid IP format
        for bad_ip in ["not-an-ip", "999.999.999.999", "10.20.30"]:
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Pod-IP": bad_ip,
                },
            )
            assert resp.status_code == 400
            assert "Invalid target IP address format." == resp.json()["detail"]

        # Loopback, link-local, multicast, unspecified IPs
        forbidden_ips = [
            "127.0.0.1",
            "::1",
            "169.254.169.254",
            "fe80::1",
            "224.0.0.1",
            "ff02::1",
            "0.0.0.0",
            "::",
        ]
        for ip in forbidden_ips:
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Pod-IP": ip,
                },
            )
            assert resp.status_code == 400
            assert "Invalid target IP address." == resp.json()["detail"]

    def test_valid_pod_ip_address_routing(self, client):
        with patch.object(
            sandbox_router.client,
            "send",
            new_callable=AsyncMock,
            side_effect=httpx.ConnectError("expected"),
        ) as mock_send:
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Pod-IP": "192.168.1.50",
                },
            )
            # Expect 502 because IP validation passes and request goes to fake backend
            assert resp.status_code == 502
            assert "Could not connect to the backend sandbox" in resp.json()["detail"]

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


class TestClusterDomain:
    def test_default_cluster_domain(self):
        assert sandbox_router.DEFAULT_CLUSTER_DOMAIN == "cluster.local"

    def test_default_when_env_var_unset(self):
        env = {k: v for k, v in os.environ.items() if k != "CLUSTER_DOMAIN"}
        with patch.dict(os.environ, env, clear=True):
            assert sandbox_router._get_cluster_domain() == "cluster.local"

    def test_env_var_overrides_cluster_domain(self):
        with patch.dict(os.environ, {"CLUSTER_DOMAIN": "my.custom.domain"}):
            assert sandbox_router._get_cluster_domain() == "my.custom.domain"

    def test_empty_env_var_falls_back_to_default(self, capsys):
        with patch.dict(os.environ, {"CLUSTER_DOMAIN": ""}):
            result = sandbox_router._get_cluster_domain()
        assert result == "cluster.local"
        captured = capsys.readouterr()
        assert "WARNING" in captured.out
        assert "CLUSTER_DOMAIN" in captured.out

    def test_module_level_cluster_domain_default(self):
        assert sandbox_router.cluster_domain == "cluster.local"

    def test_env_var_sets_module_level_cluster_domain(self):
        with patch.dict(os.environ, {"CLUSTER_DOMAIN": "my.custom.domain"}):
            importlib.reload(sandbox_router)
            assert sandbox_router.cluster_domain == "my.custom.domain"


class TestAuthentication:
    def test_auth_required_by_default_raises(self):
        with patch.dict(os.environ, {}, clear=True):
            with pytest.raises(RuntimeError, match="ROUTER_AUTH_TOKEN must be set"):
                importlib.reload(sandbox_router)

    def test_auth_disabled_by_default(self):
        with patch.dict(os.environ, {"ALLOW_UNAUTHENTICATED_ROUTER": "true"}, clear=True):
            importlib.reload(sandbox_router)
            from fastapi.testclient import TestClient
            client = TestClient(sandbox_router.app)

            with patch.object(
                sandbox_router.client,
                "send",
                new_callable=AsyncMock,
                side_effect=httpx.ConnectError("stop here")
            ):
                resp = client.post(
                    "/execute",
                    headers={"X-Sandbox-ID": "my-sandbox"},
                )
            assert resp.status_code == 502

    def test_auth_enabled_valid_token(self):
        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "secret-token"}):
            importlib.reload(sandbox_router)
            from fastapi.testclient import TestClient
            client = TestClient(sandbox_router.app)
            
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
                        "Authorization": "Bearer secret-token",
                    },
                )
            assert resp.status_code == 502

    def test_auth_enabled_missing_token(self):
        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "secret-token"}):
            importlib.reload(sandbox_router)
            from fastapi.testclient import TestClient
            client = TestClient(sandbox_router.app)
            
            resp = client.post(
                "/execute",
                headers={"X-Sandbox-ID": "my-sandbox"},
            )
            assert resp.status_code == 401
            assert "Missing or invalid Authorization header." == resp.json()["detail"]

    def test_auth_enabled_invalid_token(self):
        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "secret-token"}):
            importlib.reload(sandbox_router)
            from fastapi.testclient import TestClient
            client = TestClient(sandbox_router.app)
            
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "Authorization": "Bearer wrong-token",
                },
            )
            assert resp.status_code == 401
            assert "Invalid token." == resp.json()["detail"]

    def test_auth_enabled_whitespace_trimming(self):
        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "  secret-token\n "}):
            importlib.reload(sandbox_router)
            from fastapi.testclient import TestClient
            client = TestClient(sandbox_router.app)
            
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
                        "Authorization": "Bearer secret-token",
                    },
                )
            assert resp.status_code == 502

        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "   "}, clear=True):
            with pytest.raises(RuntimeError, match="ROUTER_AUTH_TOKEN must be set"):
                importlib.reload(sandbox_router)

    def test_custom_auth_header_preserves_authorization_for_sandbox(self):
        with patch.dict(
            os.environ,
            {
                "ROUTER_AUTH_TOKEN": "secret-token",
                "ROUTER_AUTH_HEADER": "X-Router-Authorization",
            },
            clear=True,
        ):
            importlib.reload(sandbox_router)
            from fastapi.testclient import TestClient
            client = TestClient(sandbox_router.app)

            captured_request = {}

            async def capture_send(req, **kwargs):
                captured_request["headers"] = dict(req.headers)
                raise httpx.ConnectError("stop here")

            with patch.object(sandbox_router.client, "send", side_effect=capture_send):
                resp = client.post(
                    "/execute",
                    headers={
                        "X-Sandbox-ID": "my-sandbox",
                        "X-Router-Authorization": "Bearer secret-token",
                        "Authorization": "Bearer runtime-token",
                    },
                )
            assert resp.status_code == 502
            assert captured_request["headers"]["authorization"] == "Bearer runtime-token"
            assert "x-router-authorization" not in captured_request["headers"]


class TestProxyTimeout:
    def test_default_timeout(self):
        assert sandbox_router.DEFAULT_PROXY_TIMEOUT == 180.0

    def test_env_var_overrides_timeout(self):
        with patch.dict(os.environ, {"PROXY_TIMEOUT_SECONDS": "600"}):
            importlib.reload(sandbox_router)
            assert sandbox_router.proxy_timeout == 600.0
            assert sandbox_router.client.timeout.connect == 600.0
            assert sandbox_router.client.timeout.read == 600.0

    def test_default_when_env_var_unset(self):
        with patch.dict(os.environ, {"ALLOW_UNAUTHENTICATED_ROUTER": "true"}, clear=True):
            importlib.reload(sandbox_router)
            assert sandbox_router.proxy_timeout == 180.0


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

    def test_target_url_pod_ip_construction(self, client):
        """Verify the router builds the correct URL when X-Sandbox-Pod-IP is provided."""
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
                    "X-Sandbox-Pod-IP": "10.20.30.40",
                },
            )
            built_request = mock_send.call_args
            request_obj = built_request[0][0]
            assert "10.20.30.40:9999/some/path" in str(
                request_obj.url
            )

    def test_target_url_uses_custom_cluster_domain(self, client):
        """Module-level cluster_domain should be used when constructing the target URL."""
        with patch.object(sandbox_router, "cluster_domain", "custom.domain"):
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
                request_obj = mock_send.call_args[0][0]
                assert "test-box.prod.svc.custom.domain:9999/some/path" in str(
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

    def test_authorization_header_not_forwarded(self, client):
        """The 'authorization' header should not be forwarded to the sandbox."""
        captured_request = {}

        async def capture_send(req, **kwargs):
            captured_request["headers"] = dict(req.headers)
            raise httpx.ConnectError("stop here")

        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "Authorization": "Bearer secret-token",
                },
            )
            assert "authorization" not in captured_request.get("headers", {})

    def test_routing_headers_not_forwarded(self, client):
        """Sandbox routing headers should be consumed by the router, not forwarded."""
        captured_request = {}

        async def capture_send(req, **kwargs):
            captured_request["headers"] = dict(req.headers)
            raise httpx.ConnectError("stop here")

        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Namespace": "my-ns",
                    "X-Sandbox-Port": "9999",
                    "X-Sandbox-Pod-IP": "10.20.30.40",
                },
            )
            headers = captured_request.get("headers", {})
            assert "x-sandbox-id" not in headers
            assert "x-sandbox-namespace" not in headers
            assert "x-sandbox-port" not in headers
            assert "x-sandbox-pod-ip" not in headers

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

    @patch.object(httpx.AsyncClient, "send", new_callable=AsyncMock)
    def test_request_body_streamed(self, mock_send, client):
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


class FakeUpstreamWebSocket:
    def __init__(self):
        self.sent = []
        self.closed = False
        self.messages = queue.Queue()

    async def send(self, message):
        self.sent.append(message)
        self.messages.put(f"echo:{message}" if isinstance(message, str) else message)

    async def close(self):
        self.closed = True
        self.messages.put(None)

    def __aiter__(self):
        return self

    async def __anext__(self):
        message = await asyncio.to_thread(self.messages.get)
        if message is None:
            raise StopAsyncIteration
        return message


class FakeClientWebSocket:
    def __init__(self, messages=None, *, headers=None, path="/api/ws/v2", query=""):
        self.headers = Headers(headers or {})
        self.url = URL(f"ws://router.test{path}{'?' + query if query else ''}")
        self.accepted = False
        self.closed_codes = []
        self.sent_text = []
        self.sent_bytes = []
        self.messages = queue.Queue()
        for message in messages or []:
            self.messages.put(message)

    async def accept(self):
        self.accepted = True

    async def close(self, code=1000, reason=None):
        self.closed_codes.append(code)

    async def receive(self):
        return await asyncio.to_thread(self.messages.get)

    async def send_text(self, message):
        self.sent_text.append(message)

    async def send_bytes(self, message):
        self.sent_bytes.append(message)


class TestWebSocketProxy:
    @pytest.mark.asyncio
    async def test_websocket_endpoint_connects_to_target_and_relays(self):
        upstream = FakeUpstreamWebSocket()
        captured_connect = {}

        async def fake_connect(uri, **kwargs):
            captured_connect["uri"] = uri
            captured_connect["kwargs"] = kwargs
            return upstream

        async def fake_relay(*, client_ws, upstream_ws):
            assert upstream_ws is upstream
            await upstream_ws.send("hello")

        fake_client = FakeClientWebSocket(
            headers={
                "X-Sandbox-ID": "my-sandbox",
                "X-Sandbox-Namespace": "my-ns",
                "X-Sandbox-Port": "9999",
            },
            path="/api/ws/v2",
            query="thread=abc",
        )
        with (
            patch.object(sandbox_router.websockets, "connect", side_effect=fake_connect),
            patch.object(sandbox_router, "_relay_websockets", side_effect=fake_relay),
        ):
            await sandbox_router.proxy_websocket(fake_client, "api/ws/v2")

        assert (
            captured_connect["uri"]
            == "ws://my-sandbox.my-ns.svc.cluster.local:9999/api/ws/v2?thread=abc"
        )
        assert fake_client.accepted is True
        assert upstream.sent == ["hello"]
        assert upstream.closed is True
        assert fake_client.closed_codes == [1000]

    @pytest.mark.asyncio
    async def test_websocket_preserves_authorization_with_custom_router_auth_header(self):
        with patch.dict(
            os.environ,
            {
                "ROUTER_AUTH_TOKEN": "secret-token",
                "ROUTER_AUTH_HEADER": "X-Router-Authorization",
            },
            clear=True,
        ):
            importlib.reload(sandbox_router)
            upstream = FakeUpstreamWebSocket()
            captured_connect = {}

            async def fake_connect(uri, **kwargs):
                captured_connect["uri"] = uri
                captured_connect["kwargs"] = kwargs
                return upstream

            async def fake_relay(*, client_ws, upstream_ws):
                assert upstream_ws is upstream

            fake_client = FakeClientWebSocket(
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Router-Authorization": "Bearer secret-token",
                    "Authorization": "Bearer runtime-token",
                },
            )
            with (
                patch.object(sandbox_router.websockets, "connect", side_effect=fake_connect),
                patch.object(sandbox_router, "_relay_websockets", side_effect=fake_relay),
            ):
                await sandbox_router.proxy_websocket(fake_client, "api/ws/v2")

            forwarded = dict(captured_connect["kwargs"]["additional_headers"])
            assert forwarded["authorization"] == "Bearer runtime-token"
            assert "x-router-authorization" not in forwarded

    @pytest.mark.asyncio
    async def test_websocket_rejects_missing_auth(self):
        with patch.dict(
            os.environ,
            {
                "ROUTER_AUTH_TOKEN": "secret-token",
            },
            clear=True,
        ):
            importlib.reload(sandbox_router)

            fake_client = FakeClientWebSocket(headers={"X-Sandbox-ID": "my-sandbox"})
            await sandbox_router.proxy_websocket(fake_client, "api/ws/v2")
            assert fake_client.accepted is False
            assert fake_client.closed_codes == [1008]

    @pytest.mark.asyncio
    async def test_client_to_upstream_relays_text_bytes_and_disconnect(self):
        upstream = FakeUpstreamWebSocket()
        fake_client = FakeClientWebSocket(
            [
                {"type": "websocket.receive", "text": "hello"},
                {"type": "websocket.receive", "bytes": b"data"},
                {"type": "websocket.disconnect"},
            ]
        )

        await sandbox_router._client_to_upstream(
            client_ws=fake_client,
            upstream_ws=upstream,
        )

        assert upstream.sent == ["hello", b"data"]
        assert upstream.closed is True

    @pytest.mark.asyncio
    async def test_upstream_to_client_relays_text_and_bytes_until_close(self):
        upstream = FakeUpstreamWebSocket()
        upstream.messages.put("hello")
        upstream.messages.put(b"data")
        upstream.messages.put(None)
        fake_client = FakeClientWebSocket()

        await sandbox_router._upstream_to_client(
            client_ws=fake_client,
            upstream_ws=upstream,
        )

        assert fake_client.sent_text == ["hello"]
        assert fake_client.sent_bytes == [b"data"]
