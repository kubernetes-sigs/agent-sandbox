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

"""Focused checks for token validation and REST/gRPC credential propagation."""

from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock

import grpc
import httpx
import pytest
import requests
from pydantic import ValidationError

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.models import (
    SandboxdInClusterConnectionConfig,
    SandboxdPodTunnelConnectionConfig,
    SandboxdRouterTunnelConnectionConfig,
)


@pytest.mark.parametrize("config_type, options", [
    (SandboxdPodTunnelConnectionConfig, {}),
    (SandboxdRouterTunnelConnectionConfig, {}),
    (SandboxdInClusterConnectionConfig, {"mode": "in-cluster-service"}),
])
def test_token_validation_and_redaction(config_type, options):
    assert config_type(**options).auth_token is None
    config = config_type(auth_token="private-token", **options)
    assert config.auth_token.get_secret_value() == "private-token"
    assert "private-token" not in repr(config)
    assert "private-token" not in config.model_dump_json()
    for value in ("", "private space", "private\n", "private\u00e9", b"private-bytes", 123):
        with pytest.raises(ValidationError) as caught:
            config_type(auth_token=value, **options)
        for rendered in (str(caught.value), caught.value.json(), str(caught.value.errors())):
            assert "private" not in rendered
    with pytest.raises(ValidationError) as caught:
        config_type(auth_token="private-token", rest_port=0, **options)
    assert "private-token" not in str(caught.value)


def test_model_validation_error_redacts_token():
    with pytest.raises(ValidationError) as caught:
        SandboxdInClusterConnectionConfig(
            mode="in-cluster-service", auth_token="private-token", rest_port=8080, grpc_port=8080
        )
    assert "REST and gRPC ports must be different" in str(caught.value)
    for rendered in (str(caught.value), caught.value.json(), str(caught.value.errors())):
        assert "private-token" not in rendered


@pytest.mark.parametrize("disable_retries", (False, True))
@pytest.mark.parametrize("token", ("test-token", None))
@pytest.mark.parametrize("config_type, options, token_field", [
    (SandboxdPodTunnelConnectionConfig, {}, "auth_token"),
    (SandboxdRouterTunnelConnectionConfig, {"auth_token": "sandboxd-token"}, "router_auth_token"),
    (SandboxdInClusterConnectionConfig, {"mode": "in-cluster-service"}, "auth_token"),
])
@pytest.mark.parametrize("request_options", [
    {},
    {"auth": None},
    {"auth": ("caller-user", "caller-password")},
    {"headers": {"Authorization": "Bearer caller-token"}},
])
def test_rest_auth_does_not_use_netrc(
    monkeypatch, tmp_path, disable_retries, token, config_type, options, token_field, request_options,
):
    config = config_type(**{**options, token_field: token})
    connector = SandboxConnector("sb", "ns", config, MagicMock())
    connector.connect = MagicMock(return_value="http://sandbox:8080")
    connector.strategy.verify_connection = MagicMock()
    netrc = tmp_path / ".netrc"
    netrc.write_text("default login netrc-user password netrc-password\n")
    monkeypatch.setenv("NETRC", str(netrc))
    session = connector._no_retry_session if disable_retries else connector.session
    response = requests.Response()
    response.status_code = 200
    session.send = MagicMock(return_value=response)
    try:
        connector.send_request(
            "GET", "v1/files/a", _disable_retries=disable_retries, **request_options,
        )
        authorization = session.send.call_args.args[0].headers.get("Authorization")
        assert session.trust_env
        if request_options.get("auth") is not None:
            expected = requests.Request("GET", "http://sandbox:8080", auth=request_options["auth"]).prepare()
            assert authorization == expected.headers["Authorization"]
        elif token is not None:
            assert authorization == f"Bearer {token}"
        else:
            assert authorization == request_options.get("headers", {}).get("Authorization")
    finally:
        connector.close()


@pytest.fixture
def rpc_server():
    received = []
    settings = SimpleNamespace(expected_authorization="Bearer test-token", expected_pod_ip=None)

    def reply(request, context):
        if not isinstance(request, bytes):
            # Drain the tiny upload to avoid grpc.aio's send/remote-close race.
            list(request)
        metadata = tuple(context.invocation_metadata())
        received.append(metadata)
        if dict(metadata).get("authorization") != settings.expected_authorization:
            context.abort(grpc.StatusCode.UNAUTHENTICATED, "invalid credential")
        if settings.expected_pod_ip is not None and dict(metadata).get("x-sandbox-pod-ip") != settings.expected_pod_ip:
            context.abort(grpc.StatusCode.UNAVAILABLE, "sandbox Pod is unavailable")
        return b"ok"

    def stream(request, context):
        yield reply(request, context)

    with ThreadPoolExecutor(max_workers=2) as pool:
        server = grpc.server(pool)
        shapes = ("unary_unary", "unary_stream", "stream_unary", "stream_stream")
        handlers = {
            shape: getattr(grpc, f"{shape}_rpc_method_handler")(
                stream if shape.endswith("_stream") else reply
            ) for shape in shapes
        }
        server.add_generic_rpc_handlers((grpc.method_handlers_generic_handler("test.Auth", handlers),))
        port = server.add_insecure_port("127.0.0.1:0")
        server.start()
        try:
            yield SimpleNamespace(target=f"127.0.0.1:{port}", shapes=shapes, received=received, settings=settings)
        finally:
            server.stop(0).wait(5)


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("token", ("test-token", "wrong-token", None))
@pytest.mark.parametrize("router", (False, True))
async def test_channel_authentication(rpc_server, asynchronous, token, router):
    config = (
        SandboxdRouterTunnelConnectionConfig(
            auth_token="sandboxd-token", router_auth_token=token, grpc_port=9191,
        )
        if router else SandboxdPodTunnelConnectionConfig(auth_token=token, grpc_port=9191)
    )
    if router and token is None:
        rpc_server.settings.expected_authorization = None
    client_type = AsyncSandboxConnector if asynchronous else SandboxConnector
    connector = client_type("sb", "ns", config, MagicMock())
    # Address discovery is irrelevant to authentication; channels remain real.
    if asynchronous:
        connector.grpc_target = rpc_server.target
        channel = await connector.grpc_channel()
    else:
        connector.strategy.grpc_target = rpc_server.target
        channel = connector.grpc_channel()
    metadata = (
        ("x-request-id", "request"), ("x-request-id", "second"),
        ("custom-bin", b"\x00\xff"),
    )
    if router:
        metadata += (
            ("x-sandbox-id", "caller-override"),
            ("x-sandbox-namespace", "caller-override"),
            ("x-sandbox-port", "1"),
            ("x-sandboxd-token", "caller-override"),
        )
    if token is not None:
        metadata += (("authorization", "caller-override"),)

    async def invoke(shape):
        request = iter((b"request",)) if shape.startswith("stream_") else b"request"
        call = getattr(channel, shape)(f"/test.Auth/{shape}")(request, metadata=metadata, timeout=5)
        if shape.endswith("_stream"):
            return [part async for part in call] if asynchronous else list(call)
        return await call if asynchronous else call

    try:
        succeeds = token == "test-token" or (router and token is None)
        shapes = rpc_server.shapes if succeeds else ("unary_unary",)
        for shape in shapes:
            if succeeds:
                assert await invoke(shape) == ([b"ok"] if shape.endswith("_stream") else b"ok")
            else:
                with pytest.raises(grpc.RpcError) as caught:
                    await invoke(shape)
                assert caught.value.code() == grpc.StatusCode.UNAUTHENTICATED
            received = rpc_server.received[-1]
            expected = [] if token is None else [f"Bearer {token}"]
            assert [value for key, value in received if key == "authorization"] == expected
            assert [value for key, value in received if key == "x-sandboxd-token"] == (
                ["sandboxd-token"] if router else []
            )
            assert ("x-request-id", "request") in received
            assert ("x-request-id", "second") in received
            assert ("custom-bin", b"\x00\xff") in received
            for key, value in (("x-sandbox-id", "sb"), ("x-sandbox-namespace", "ns"), ("x-sandbox-port", "9191")):
                assert [item for name, item in received if name == key] == ([value] if router else [])
        assert len(rpc_server.received) == len(shapes)
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("token", ("test-token", None))
@pytest.mark.parametrize("router", (False, True))
async def test_rest_authentication(asynchronous, token, router):
    config = (
        SandboxdRouterTunnelConnectionConfig(
            auth_token="sandboxd-token", router_auth_token=token, rest_port=8181,
        )
        if router else SandboxdInClusterConnectionConfig(mode="in-cluster-service", auth_token=token)
    )
    client_type = AsyncSandboxConnector if asynchronous else SandboxConnector
    connector = client_type("sb", "ns", config, MagicMock())
    if asynchronous:
        connector._resolve_base_url = AsyncMock(return_value="http://sandbox:8080")
        response = httpx.Response(200, request=httpx.Request("GET", "http://sandbox:8080/v1/files/a"))
        send = connector.client.request = AsyncMock(return_value=response)
    else:
        connector.connect = MagicMock(return_value="http://sandbox:8080")
        connector.strategy.verify_connection = MagicMock()
        send = connector.session.request = MagicMock(return_value=MagicMock(status_code=200, is_redirect=False))
    try:
        headers = {"AUTHORIZATION": "caller-override"} if token else {}
        if router:
            headers.update({
                "x-sandbox-id": "wrong", "x-sandbox-namespace": "wrong", "x-sandbox-port": "1",
                "X-Sandboxd-Token": "wrong",
            })
        response = connector.send_request("GET", "v1/files/a", headers=headers)
        if asynchronous:
            response = await response
        assert response.status_code == 200
        sent = {key.lower(): value for key, value in send.call_args.kwargs["headers"].items()}
        if router:
            assert connector.is_sandboxd()
            assert sent["x-sandbox-id"] == "sb"
            assert sent["x-sandbox-namespace"] == "ns"
            assert sent["x-sandbox-port"] == "8181"
            assert len([key for key in send.call_args.kwargs["headers"] if key.lower() == "x-sandbox-port"]) == 1
        assert sent.get("x-sandboxd-token") == ("sandboxd-token" if router else None)
        assert sent.get("authorization") == (f"Bearer {token}" if token else None)
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("pod_ip_routing", (False, True))
async def test_router_pod_ip_routing(rpc_server, asynchronous, pod_ip_routing):
    rpc_server.settings.expected_authorization = None
    get_pod_ip = AsyncMock(return_value="10.0.0.8") if asynchronous else MagicMock(return_value="10.0.0.8")
    config = SandboxdRouterTunnelConnectionConfig(pod_ip_routing=pod_ip_routing, grpc_port=9191)
    client_type = AsyncSandboxConnector if asynchronous else SandboxConnector
    connector = client_type("sb", "ns", config, MagicMock(), get_pod_ip=get_pod_ip)
    if asynchronous:
        connector.grpc_target = rpc_server.target
        connector._resolve_base_url = AsyncMock(return_value="http://sandbox:8080")
        response = httpx.Response(200, request=httpx.Request("GET", "http://sandbox:8080/v1/files/a"))
        send = connector.client.request = AsyncMock(return_value=response)
    else:
        connector.strategy.grpc_target = rpc_server.target
        connector.connect = MagicMock(return_value="http://sandbox:8080")
        connector.strategy.verify_connection = MagicMock()
        send = connector.session.request = MagicMock(return_value=MagicMock(status_code=200, is_redirect=False))
    try:
        response = connector.send_request("GET", "v1/files/a")
        if asynchronous:
            response = await response
        assert response.status_code == 200
        sent = {key.lower(): value for key, value in send.call_args.kwargs["headers"].items()}
        channel = await connector.grpc_channel() if asynchronous else connector.grpc_channel()
        call = channel.unary_unary("/test.Auth/unary_unary")(b"request", timeout=5)
        assert (await call if asynchronous else call) == b"ok"
        received = [value for key, value in rpc_server.received[-1] if key == "x-sandbox-pod-ip"]
        if pod_ip_routing:
            assert sent["x-sandbox-pod-ip"] == "10.0.0.8"
            assert received == ["10.0.0.8"]
        else:
            assert "x-sandbox-pod-ip" not in sent
            assert received == []
            if asynchronous:
                get_pod_ip.assert_not_awaited()
            else:
                get_pod_ip.assert_not_called()
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("failure", ("grpc", "http"))
async def test_router_grpc_refreshes_pod_ip_after_failure(rpc_server, asynchronous, failure):
    rpc_server.settings.expected_authorization = None
    rpc_server.settings.expected_pod_ip = "10.0.0.8"
    get_pod_ip = AsyncMock(return_value="10.0.0.8") if asynchronous else MagicMock(return_value="10.0.0.8")
    config = SandboxdRouterTunnelConnectionConfig(pod_ip_routing=True)
    client_type = AsyncSandboxConnector if asynchronous else SandboxConnector
    connector = client_type("sb", "ns", config, MagicMock(), get_pod_ip=get_pod_ip)
    if asynchronous:
        connector.grpc_target = rpc_server.target
        connector._resolve_base_url = AsyncMock(return_value="http://sandbox:8080")
        response = httpx.Response(502, request=httpx.Request("GET", "http://sandbox:8080/v1/files/a"))
        connector.client.request = AsyncMock(return_value=response)
    else:
        connector.strategy.grpc_target = rpc_server.target
        connector.connect = MagicMock(return_value="http://sandbox:8080")
        connector.strategy.verify_connection = MagicMock()
        response = requests.Response()
        response.status_code = 502
        connector._no_retry_session.request = MagicMock(return_value=response)

    async def get_channel():
        return await connector.grpc_channel() if asynchronous else connector.grpc_channel()

    async def invoke(channel):
        call = channel.unary_unary("/test.Auth/unary_unary")(b"request", timeout=5)
        return await call if asynchronous else call

    async def invalidate(channel):
        result = connector.invalidate_sandboxd_transport(channel)
        if asynchronous:
            await result

    try:
        first_channel = await get_channel()
        assert await invoke(first_channel) == b"ok"
        assert await get_channel() is first_channel
        get_pod_ip.return_value = "10.0.0.9"
        rpc_server.settings.expected_pod_ip = "10.0.0.9"

        if failure == "grpc":
            with pytest.raises(grpc.RpcError) as caught:
                await invoke(first_channel)
            assert caught.value.code() == grpc.StatusCode.UNAVAILABLE
            await invalidate(first_channel)
        else:
            with pytest.raises(SandboxRequestError) as caught:
                result = connector.send_request("GET", "v1/files/a", _disable_retries=True)
                if asynchronous:
                    await result
            assert caught.value.status_code == 502
            response.status_code = 200
            result = connector.send_request("GET", "v1/files/a", _disable_retries=True)
            if asynchronous:
                await result
            send = connector.client.request if asynchronous else connector._no_retry_session.request
            sent = {key.lower(): value for key, value in send.call_args.kwargs["headers"].items()}
            assert sent["x-sandbox-pod-ip"] == "10.0.0.9"

        # The router endpoint stays the same when the Sandbox gets a new Pod IP.
        channel = await get_channel()
        assert channel is not first_channel
        assert await invoke(channel) == b"ok"
        assert dict(rpc_server.received[-1])["x-sandbox-pod-ip"] == "10.0.0.9"
        assert len(rpc_server.received) == (3 if failure == "grpc" else 2)

        await invalidate(first_channel)
        assert await get_channel() is channel
        assert (get_pod_ip.await_count if asynchronous else get_pod_ip.call_count) == 2
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()


def test_router_auth_token_redaction():
    config = SandboxdRouterTunnelConnectionConfig(router_auth_token="private-token")
    assert config.router_auth_token.get_secret_value() == "private-token"
    assert "private-token" not in repr(config)
    assert "private-token" not in config.model_dump_json()
    for value in ("", "private space", "private\n", "private\u00e9"):
        with pytest.raises(ValidationError) as caught:
            SandboxdRouterTunnelConnectionConfig(router_auth_token=value)
        for rendered in (str(caught.value), caught.value.json(), str(caught.value.errors())):
            assert "private" not in rendered


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("router_token", ("router-token", None))
async def test_router_sends_sandboxd_auth_token_separately(rpc_server, asynchronous, router_token):
    config = SandboxdRouterTunnelConnectionConfig(
        auth_token="sandboxd-token", router_auth_token=router_token, grpc_port=9191,
    )
    authorization = f"Bearer {router_token}" if router_token else None
    rpc_server.settings.expected_authorization = authorization
    client_type = AsyncSandboxConnector if asynchronous else SandboxConnector
    connector = client_type("sb", "ns", config, MagicMock())
    if asynchronous:
        connector.grpc_target = rpc_server.target
        connector._resolve_base_url = AsyncMock(return_value="http://sandbox:8080")
        response = httpx.Response(200, request=httpx.Request("GET", "http://sandbox:8080/v1/files/a"))
        send = connector.client.request = AsyncMock(return_value=response)
        channel = await connector.grpc_channel()
    else:
        connector.strategy.grpc_target = rpc_server.target
        connector.connect = MagicMock(return_value="http://sandbox:8080")
        connector.strategy.verify_connection = MagicMock()
        send = connector.session.request = MagicMock(return_value=MagicMock(status_code=200, is_redirect=False))
        channel = connector.grpc_channel()
    try:
        metadata = (("authorization", "caller-override"),) if router_token else ()
        call = channel.unary_unary("/test.Auth/unary_unary")(
            b"request", metadata=metadata, timeout=5,
        )
        assert (await call if asynchronous else call) == b"ok"
        received = rpc_server.received[-1]
        expected = [authorization] if authorization else []
        assert [value for key, value in received if key == "authorization"] == expected
        assert [value for key, value in received if key == "x-sandboxd-token"] == ["sandboxd-token"]
        assert all("sandboxd-token" not in str(value) for key, value in received if key != "x-sandboxd-token")

        headers = {"AUTHORIZATION": "caller-override"} if router_token else {}
        response = connector.send_request(
            "GET", "v1/files/a", headers=headers,
        )
        if asynchronous:
            response = await response
        assert response.status_code == 200
        sent = {key.lower(): value for key, value in send.call_args.kwargs["headers"].items()}
        assert sent.get("authorization") == authorization
        assert sent["x-sandboxd-token"] == "sandboxd-token"
        assert all("sandboxd-token" not in value for key, value in sent.items() if key != "x-sandboxd-token")
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()
