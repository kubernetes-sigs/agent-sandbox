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
import ipaddress
import os
import time
from contextlib import asynccontextmanager
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest
from fastapi.testclient import TestClient
from starlette.websockets import WebSocketDisconnect

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


@pytest.fixture
def isolated_sandbox_router():
    """Reload sandbox_router under a patched env and restore module state after."""
    saved_env = dict(os.environ)

    def _reload(**env_vars):
        env = {"ALLOW_UNAUTHENTICATED_ROUTER": "true", **env_vars}
        with patch.dict(os.environ, env, clear=True):
            importlib.reload(sandbox_router)
        return sandbox_router

    yield _reload

    os.environ.clear()
    os.environ.update(saved_env)
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

    def test_request_header_below_proxy_timeout_overrides_default(self, client):
        async def capture_send(req, **kwargs):
            capture_send.timeout = req.extensions.get("timeout")
            raise httpx.ConnectError("stop here")

        capture_send.timeout = None
        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Timeout": "60",
                },
            )

        assert resp.status_code == 502
        assert capture_send.timeout["connect"] == 5.0
        assert capture_send.timeout["read"] == 60.0

    def test_invalid_request_header_falls_back_to_default_timeout(self, client):
        async def capture_send(req, **kwargs):
            capture_send.timeout = req.extensions.get("timeout")
            raise httpx.ConnectError("stop here")

        capture_send.timeout = None
        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Timeout": "invalid",
                },
            )

        assert resp.status_code == 502
        assert capture_send.timeout["connect"] == min(sandbox_router.proxy_timeout, 5.0)
        assert capture_send.timeout["read"] == sandbox_router.proxy_timeout

    def test_non_finite_request_header_falls_back_to_default_timeout(self, client):
        async def capture_send(req, **kwargs):
            capture_send.timeout = req.extensions.get("timeout")
            raise httpx.ConnectError("stop here")

        capture_send.timeout = None
        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Timeout": "inf",
                },
            )

        assert resp.status_code == 502
        assert capture_send.timeout["connect"] == min(sandbox_router.proxy_timeout, 5.0)
        assert capture_send.timeout["read"] == sandbox_router.proxy_timeout

    @pytest.mark.parametrize("timeout_value", ["0", "-1"])
    def test_non_positive_request_header_falls_back_to_default_timeout(
        self, client, timeout_value
    ):
        async def capture_send(req, **kwargs):
            capture_send.timeout = req.extensions.get("timeout")
            raise httpx.ConnectError("stop here")

        capture_send.timeout = None
        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Timeout": timeout_value,
                },
            )

        assert resp.status_code == 502
        assert capture_send.timeout["connect"] == min(sandbox_router.proxy_timeout, 5.0)
        assert capture_send.timeout["read"] == sandbox_router.proxy_timeout

    def test_request_header_above_proxy_timeout_is_capped(self, client):
        async def capture_send(req, **kwargs):
            capture_send.timeout = req.extensions.get("timeout")
            raise httpx.ConnectError("stop here")

        capture_send.timeout = None
        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            resp = client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Timeout": "600",
                },
            )

        assert resp.status_code == 502
        assert capture_send.timeout["connect"] == min(sandbox_router.proxy_timeout, 5.0)
        assert capture_send.timeout["read"] == sandbox_router.proxy_timeout


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

    def test_timeout_error_returns_504(self, client):
        """When the target sandbox times out, the router should return 504."""
        with patch.object(
            sandbox_router.client,
            "send",
            new_callable=AsyncMock,
            side_effect=httpx.TimeoutException("timed out"),
        ):
            resp = client.post(
                "/execute",
                headers={"X-Sandbox-ID": "slow-sandbox"},
                content=b'{"command": "sleep 999"}',
            )
            assert resp.status_code == 504
            assert "slow-sandbox" in resp.json()["detail"]

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

    def test_target_url_ipv6_pod_ip_construction(self, client):
        """IPv6 pod IPs must be bracketed in the upstream URL (RFC 3986)."""
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
                    "X-Sandbox-Pod-IP": "2001:db8::1",
                },
            )
            built_request = mock_send.call_args
            request_obj = built_request[0][0]
            assert "[2001:db8::1]:9999/some/path" in str(
                request_obj.url
            )

    def test_target_url_ipv6_full_form_pod_ip_construction(self, client):
        """Full-form IPv6 addresses are normalized and bracketed."""
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
                    "X-Sandbox-Pod-IP": "2001:0db8:0000:0000:0000:0000:0000:0001",
                },
            )
            built_request = mock_send.call_args
            request_obj = built_request[0][0]
            assert "[2001:db8::1]:9999/some/path" in str(
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

    def test_connection_nominated_headers_not_forwarded(self, client):
        """Headers named by Connection must not be forwarded to the sandbox."""
        captured_request = {}

        async def capture_send(req, **kwargs):
            captured_request["headers"] = dict(req.headers)
            raise httpx.ConnectError("stop here")

        with patch.object(sandbox_router.client, "send", side_effect=capture_send):
            client.post(
                "/execute",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "Connection": "close, X-Custom-Hop",
                    "X-Custom-Hop": "should-not-forward",
                    "X-App-Header": "keep-me",
                },
            )
            forwarded = {
                key.lower(): value
                for key, value in captured_request.get("headers", {}).items()
            }
            assert "x-custom-hop" not in forwarded
            assert forwarded.get("x-app-header") == "keep-me"

    def test_proxy_headers_strip_connection_nominated_headers(self):
        forwarded = sandbox_router._proxy_headers({
            "connection": "close, X-Custom-Hop",
            "X-Custom-Hop": "secret",
            "X-Forwarded-For": "203.0.113.10",
        })
        lowered = {key.lower() for key in forwarded}
        assert "x-custom-hop" not in lowered
        assert "connection" not in lowered
        assert forwarded.get("X-Forwarded-For") == "203.0.113.10"

    def test_response_headers_strip_connection_nominated_headers(self):
        forwarded = sandbox_router._response_headers({
            "connection": "Foo",
            "Foo": "bar",
            "Content-Type": "application/json",
        })
        assert forwarded == {"Content-Type": "application/json"}

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

    def test_websocket_upgrade_over_http_returns_502(self, client):
        """101 Switching Protocols cannot be forwarded over plain HTTP."""
        mock_resp = AsyncMock(spec=httpx.Response)
        mock_resp.status_code = 101
        mock_resp.headers = {}
        mock_resp.aclose = AsyncMock()

        with patch.object(
            sandbox_router.client,
            "send",
            new_callable=AsyncMock,
            return_value=mock_resp,
        ):
            resp = client.get(
                "/kernels",
                headers={"X-Sandbox-ID": "my-sandbox"},
            )

        assert resp.status_code == 502
        assert "WebSocket" in resp.json()["detail"]
        mock_resp.aclose.assert_awaited_once()


class TestWebSocketProxyValidation:
    def test_missing_sandbox_id_header(self, client):
        with pytest.raises(WebSocketDisconnect) as exc_info:
            with client.websocket_connect("/kernels"):
                pass
        assert exc_info.value.code == 1008
        assert exc_info.value.reason == "X-Sandbox-ID header is required."

    def test_invalid_namespace_format(self, client):
        with pytest.raises(WebSocketDisconnect) as exc_info:
            with client.websocket_connect(
                "/kernels",
                headers={
                    "X-Sandbox-ID": "my-sandbox",
                    "X-Sandbox-Namespace": "bad namespace!",
                },
            ):
                pass
        assert exc_info.value.code == 1008
        assert exc_info.value.reason == "Invalid namespace format."

    def test_missing_auth_token(self):
        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "secret-token"}, clear=True):
            importlib.reload(sandbox_router)
            client = TestClient(sandbox_router.app)

            with pytest.raises(WebSocketDisconnect) as exc_info:
                with client.websocket_connect(
                    "/kernels",
                    headers={"X-Sandbox-ID": "my-sandbox"},
                ):
                    pass
            assert exc_info.value.code == 1008
            assert exc_info.value.reason == "Missing or invalid Authorization header."

    def test_invalid_auth_token(self):
        with patch.dict(os.environ, {"ROUTER_AUTH_TOKEN": "secret-token"}, clear=True):
            importlib.reload(sandbox_router)
            client = TestClient(sandbox_router.app)

            with pytest.raises(WebSocketDisconnect) as exc_info:
                with client.websocket_connect(
                    "/kernels",
                    headers={
                        "X-Sandbox-ID": "my-sandbox",
                        "Authorization": "Bearer wrong-token",
                    },
                ):
                    pass
            assert exc_info.value.code == 1008
            assert exc_info.value.reason == "Invalid token."


class _MockBackendWebSocket:
    """Minimal async-iterable stand-in for a backend websockets connection."""

    def __init__(self, messages=None) -> None:
        self.subprotocol = None
        self._messages = list(messages or [])
        self._index = 0
        self.close_code = None
        self.close_reason = None
        self.closed_with: tuple[int, str] | None = None

    def __aiter__(self):
        return self

    async def __anext__(self):
        if self._index < len(self._messages):
            message = self._messages[self._index]
            self._index += 1
            return message
        await asyncio.sleep(3600)
        raise StopAsyncIteration

    async def send(self, _message) -> None:
        return None

    async def close(self, code: int = 1000, reason: str = "") -> None:
        self.closed_with = (code, reason)


def _mock_backend_websocket(messages=None):
    """Return an async context manager that yields a mock backend WebSocket."""
    backend_ws = _MockBackendWebSocket(messages)

    @asynccontextmanager
    async def _connect(*_args, **_kwargs):
        yield backend_ws

    return _connect, backend_ws


class TestWebSocketResourceLimits:
    def test_default_websocket_limits(self, isolated_sandbox_router):
        mod = isolated_sandbox_router()
        assert mod.DEFAULT_WEBSOCKET_IDLE_TIMEOUT == 3600.0
        assert mod.DEFAULT_WEBSOCKET_MAX_LIFETIME == 86400.0
        assert mod.DEFAULT_WEBSOCKET_MAX_CONNECTIONS_PER_CLIENT == 64
        assert mod.DEFAULT_WEBSOCKET_MAX_MESSAGE_BYTES == 16 * 1024 * 1024
        assert mod.websocket_idle_timeout == 3600.0
        assert mod.websocket_max_lifetime == 86400.0
        assert mod.websocket_max_connections_per_client == 64
        assert mod.websocket_max_message_bytes == 16 * 1024 * 1024

    def test_env_vars_override_websocket_limits(self, isolated_sandbox_router):
        mod = isolated_sandbox_router(
            WEBSOCKET_IDLE_TIMEOUT_SECONDS="120",
            WEBSOCKET_MAX_LIFETIME_SECONDS="600",
            WEBSOCKET_MAX_CONNECTIONS_PER_CLIENT="8",
            WEBSOCKET_MAX_MESSAGE_BYTES="33554432",
        )
        assert mod.websocket_idle_timeout == 120.0
        assert mod.websocket_max_lifetime == 600.0
        assert mod.websocket_max_connections_per_client == 8
        assert mod.websocket_max_message_bytes == 33554432

    def test_zero_disables_websocket_limits(self, isolated_sandbox_router):
        mod = isolated_sandbox_router(
            WEBSOCKET_IDLE_TIMEOUT_SECONDS="0",
            WEBSOCKET_MAX_LIFETIME_SECONDS="0",
            WEBSOCKET_MAX_CONNECTIONS_PER_CLIENT="0",
        )
        assert mod.websocket_idle_timeout == 0.0
        assert mod.websocket_max_lifetime == 0.0
        assert mod.websocket_max_connections_per_client == 0

    def test_fractional_connection_limit_falls_back_to_default(self, isolated_sandbox_router):
        mod = isolated_sandbox_router(WEBSOCKET_MAX_CONNECTIONS_PER_CLIENT="0.5")
        assert mod.websocket_max_connections_per_client == 64

    def test_invalid_message_size_falls_back_to_default(self, isolated_sandbox_router):
        mod = isolated_sandbox_router(WEBSOCKET_MAX_MESSAGE_BYTES="0")
        assert mod.websocket_max_message_bytes == 16 * 1024 * 1024

    def test_backend_websocket_connect_uses_configured_max_message_bytes(self):
        captured_kwargs: dict = {}

        @asynccontextmanager
        async def capture_connect(*_args, **kwargs):
            captured_kwargs.update(kwargs)
            yield _MockBackendWebSocket()

        with patch.dict(
            os.environ,
            {"ALLOW_UNAUTHENTICATED_ROUTER": "true"},
            clear=True,
        ):
            importlib.reload(sandbox_router)
            client = TestClient(sandbox_router.app)
            with patch("sandbox_router.websockets.connect", side_effect=capture_connect):
                with client.websocket_connect(
                    "/kernels",
                    headers={"X-Sandbox-ID": "my-sandbox"},
                ):
                    pass
            assert captured_kwargs.get("max_size") == 16 * 1024 * 1024

    def test_client_connection_key_uses_forwarded_for_from_trusted_proxy(self):
        websocket = MagicMock()
        websocket.headers = {"x-forwarded-for": "203.0.113.10, 10.0.0.1"}
        websocket.client.host = "10.0.0.5"
        trusted = (ipaddress.ip_network("10.0.0.0/8"),)
        assert (
            sandbox_router._client_connection_key(
                websocket,
                trusted_networks=trusted,
            )
            == "203.0.113.10"
        )

    def test_client_connection_key_ignores_spoofed_forwarded_for_prefix(self):
        """A client-supplied leftmost X-Forwarded-For hop must not win over appended hops."""
        websocket = MagicMock()
        websocket.headers = {"x-forwarded-for": "203.0.113.1, 203.0.113.10"}
        websocket.client.host = "10.0.0.5"
        trusted = (ipaddress.ip_network("10.0.0.0/8"),)
        assert (
            sandbox_router._client_connection_key(
                websocket,
                trusted_networks=trusted,
            )
            == "203.0.113.10"
        )

    def test_client_connection_key_ignores_forwarded_for_from_untrusted_peer(self):
        websocket = MagicMock()
        websocket.headers = {"x-forwarded-for": "203.0.113.10"}
        websocket.client.host = "203.0.113.99"
        trusted = (ipaddress.ip_network("10.0.0.0/8"),)
        assert (
            sandbox_router._client_connection_key(
                websocket,
                trusted_networks=trusted,
            )
            == "203.0.113.99"
        )

    def test_client_connection_key_ignores_forwarded_for_when_no_trusted_proxies(self):
        websocket = MagicMock()
        websocket.headers = {"x-forwarded-for": "203.0.113.10"}
        websocket.client.host = "127.0.0.1"
        assert (
            sandbox_router._client_connection_key(
                websocket,
                trusted_networks=(),
            )
            == "127.0.0.1"
        )

    def test_parse_trusted_proxy_networks_skips_invalid_entries(self, capsys):
        networks = sandbox_router._parse_trusted_proxy_networks(
            "10.0.0.0/8, not-a-cidr, 192.168.0.0/16"
        )
        assert networks == (
            ipaddress.ip_network("10.0.0.0/8"),
            ipaddress.ip_network("192.168.0.0/16"),
        )
        captured = capsys.readouterr()
        assert "not-a-cidr" in captured.out

    def test_connection_limit_rejects_excess_connections(self):
        with patch.dict(
            os.environ,
            {
                "ALLOW_UNAUTHENTICATED_ROUTER": "true",
                "WEBSOCKET_MAX_CONNECTIONS_PER_CLIENT": "1",
            },
            clear=True,
        ):
            importlib.reload(sandbox_router)
            client = TestClient(sandbox_router.app)
            connect, _backend_ws = _mock_backend_websocket()

            with patch("sandbox_router.websockets.connect", side_effect=connect):
                with client.websocket_connect(
                    "/kernels",
                    headers={"X-Sandbox-ID": "my-sandbox"},
                ):
                    with pytest.raises(WebSocketDisconnect) as exc_info:
                        with client.websocket_connect(
                            "/kernels",
                            headers={"X-Sandbox-ID": "my-sandbox"},
                        ):
                            pass
                    assert exc_info.value.code == 1008
                    assert "Too many concurrent WebSocket connections" in exc_info.value.reason

    def test_idle_timeout_closes_relay(self):
        client_ws = AsyncMock()

        async def hang_on_receive() -> None:
            await asyncio.Event().wait()

        client_ws.receive = hang_on_receive
        backend_ws = _MockBackendWebSocket()

        async def run_relay() -> None:
            await sandbox_router._relay_websocket(
                client_ws,
                backend_ws,
                idle_timeout=0.2,
                max_lifetime=0.0,
            )

        started_at = time.monotonic()
        asyncio.run(run_relay())
        elapsed = time.monotonic() - started_at
        assert 0.1 <= elapsed < 2.0
        client_ws.close.assert_awaited_once_with(
            code=1001,
            reason="idle timeout exceeded",
        )

    def test_oversized_backend_message_closes_client_with_1009(self):
        from websockets.exceptions import PayloadTooBig

        class OversizedBackendWebSocket(_MockBackendWebSocket):
            def __aiter__(self):
                return self

            async def __anext__(self):
                raise PayloadTooBig(100, 200)

        client_ws = AsyncMock()

        async def hang_on_receive() -> None:
            await asyncio.Event().wait()

        client_ws.receive = hang_on_receive
        backend_ws = OversizedBackendWebSocket()

        async def run_relay() -> None:
            await sandbox_router._relay_websocket(
                client_ws,
                backend_ws,
                idle_timeout=0.0,
                max_lifetime=0.0,
            )

        asyncio.run(run_relay())
        client_ws.close.assert_awaited_once_with(
            code=1009,
            reason="message too big",
        )
        assert backend_ws.closed_with == (1009, "message too big")


class TestWebSocketRelayClose:
    def test_backend_close_code_propagates_to_client(self):
        from websockets.exceptions import ConnectionClosed
        from websockets.frames import Close

        class ClosingBackendWebSocket(_MockBackendWebSocket):
            async def __anext__(self):
                self.close_code = 1011
                self.close_reason = "kernel restart"
                raise ConnectionClosed(Close(1011, "kernel restart"), None)

        client_ws = AsyncMock()

        async def hang_on_receive() -> None:
            await asyncio.Event().wait()

        client_ws.receive = hang_on_receive
        backend_ws = ClosingBackendWebSocket()

        async def run_relay() -> None:
            await sandbox_router._relay_websocket(
                client_ws,
                backend_ws,
                idle_timeout=0.0,
                max_lifetime=0.0,
            )

        asyncio.run(run_relay())
        client_ws.close.assert_awaited_once_with(
            code=1011,
            reason="kernel restart",
        )

    def test_client_close_code_propagates_to_backend(self):
        client_ws = AsyncMock()
        client_ws.receive = AsyncMock(
            return_value={
                "type": "websocket.disconnect",
                "code": 4001,
                "reason": "auth failed",
            },
        )
        backend_ws = _MockBackendWebSocket()

        async def run_relay() -> None:
            await sandbox_router._relay_websocket(
                client_ws,
                backend_ws,
                idle_timeout=0.0,
                max_lifetime=0.0,
            )

        asyncio.run(run_relay())
        assert backend_ws.closed_with == (4001, "auth failed")


class TestMaxKeepaliveConnections:
    def test_default_max_keepalive_connections(self):
        assert sandbox_router.DEFAULT_MAX_KEEPALIVE_CONNECTIONS == 20

    def test_env_var_overrides(self):
        with patch.dict(os.environ, {"MAX_KEEPALIVE_CONNECTIONS": "50"}):
            importlib.reload(sandbox_router)
            assert sandbox_router.max_keepalive_connections == 50

    def test_default_when_env_var_unset(self):
        with patch.dict(os.environ, {"ALLOW_UNAUTHENTICATED_ROUTER": "true"}, clear=True):
            importlib.reload(sandbox_router)
            assert sandbox_router.max_keepalive_connections == 20

    def test_invalid_env_var_falls_back_to_default(self, capsys):
        with patch.dict(os.environ, {"MAX_KEEPALIVE_CONNECTIONS": "not-a-number"}):
            result = sandbox_router._get_max_keepalive_connections()
        assert result == 20
        captured = capsys.readouterr()
        assert "WARNING" in captured.out
        assert "MAX_KEEPALIVE_CONNECTIONS" in captured.out

    def test_negative_env_var_falls_back_to_default(self, capsys):
        with patch.dict(os.environ, {"MAX_KEEPALIVE_CONNECTIONS": "-1"}):
            result = sandbox_router._get_max_keepalive_connections()
        assert result == 20
        captured = capsys.readouterr()
        assert "WARNING" in captured.out
        assert "MAX_KEEPALIVE_CONNECTIONS" in captured.out

    def test_zero_disables_pooling(self):
        with patch.dict(os.environ, {"MAX_KEEPALIVE_CONNECTIONS": "0"}):
            result = sandbox_router._get_max_keepalive_connections()
        assert result == 0
