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
from k8s_agent_sandbox.models import SandboxdInClusterConnectionConfig, SandboxdPodTunnelConnectionConfig


@pytest.mark.parametrize("config_type, options", [
    (SandboxdPodTunnelConnectionConfig, {}),
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
def test_rest_token_takes_precedence_over_netrc(monkeypatch, disable_retries, token):
    config = SandboxdPodTunnelConnectionConfig(auth_token=token)
    connector = SandboxConnector("sb", "ns", config, MagicMock())
    connector.connect = MagicMock(return_value="http://sandbox:8080")
    connector.strategy.verify_connection = MagicMock()
    netrc = MagicMock(return_value=("netrc-user", "netrc-password"))
    monkeypatch.setattr("requests.sessions.get_netrc_auth", netrc)
    session = connector._no_retry_session if disable_retries else connector.session
    response = requests.Response()
    response.status_code = 200
    session.send = MagicMock(return_value=response)
    try:
        connector.send_request("GET", "v1/files/a", _disable_retries=disable_retries)
        authorization = session.send.call_args.args[0].headers["Authorization"]
        assert session.trust_env
        if token is not None:
            assert authorization == f"Bearer {token}"
            netrc.assert_not_called()
        else:
            assert authorization.startswith("Basic ")
            netrc.assert_called_once()
    finally:
        connector.close()


@pytest.fixture
def rpc_server():
    received = []

    def reply(request, context):
        if not isinstance(request, bytes):
            # Drain the tiny upload to avoid grpc.aio's send/remote-close race.
            list(request)
        metadata = tuple(context.invocation_metadata())
        received.append(metadata)
        if dict(metadata).get("authorization") != "Bearer test-token":
            context.abort(grpc.StatusCode.UNAUTHENTICATED, "invalid credential")
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
            yield SimpleNamespace(target=f"127.0.0.1:{port}", shapes=shapes, received=received)
        finally:
            server.stop(0).wait(5)


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("token", ("test-token", "wrong-token", None))
async def test_channel_authentication(rpc_server, asynchronous, token):
    config = SandboxdPodTunnelConnectionConfig(auth_token=token)
    client_type = AsyncSandboxConnector if asynchronous else SandboxConnector
    connector = client_type("sb", "ns", config, MagicMock())
    # Address discovery is irrelevant to authentication; channels remain real.
    if asynchronous:
        connector.grpc_target = rpc_server.target
        channel = await connector.grpc_channel()
    else:
        connector.strategy.grpc_target = rpc_server.target
        channel = connector.grpc_channel()
    metadata = (("x-request-id", "request"),)
    if token is not None:
        metadata += (("authorization", "caller-override"),)

    async def invoke(shape):
        request = iter((b"request",)) if shape.startswith("stream_") else b"request"
        call = getattr(channel, shape)(f"/test.Auth/{shape}")(request, metadata=metadata, timeout=5)
        if shape.endswith("_stream"):
            return [part async for part in call] if asynchronous else list(call)
        return await call if asynchronous else call

    try:
        shapes = rpc_server.shapes if token == "test-token" else ("unary_unary",)
        for shape in shapes:
            if token == "test-token":
                assert await invoke(shape) == ([b"ok"] if shape.endswith("_stream") else b"ok")
            else:
                with pytest.raises(grpc.RpcError) as caught:
                    await invoke(shape)
                assert caught.value.code() == grpc.StatusCode.UNAUTHENTICATED
            received = rpc_server.received[-1]
            expected = [] if token is None else [f"Bearer {token}"]
            assert [value for key, value in received if key == "authorization"] == expected
            assert ("x-request-id", "request") in received
        assert len(rpc_server.received) == len(shapes)
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("asynchronous", (False, True))
@pytest.mark.parametrize("token", ("test-token", None))
async def test_rest_authentication(asynchronous, token):
    config = SandboxdInClusterConnectionConfig(mode="in-cluster-service", auth_token=token)
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
        response = connector.send_request("GET", "v1/files/a", headers=headers)
        if asynchronous:
            response = await response
        assert response.status_code == 200
        sent = {key.lower(): value for key, value in send.call_args.kwargs["headers"].items()}
        assert sent.get("authorization") == (f"Bearer {token}" if token else None)
    finally:
        if asynchronous:
            await connector.close()
        else:
            connector.close()
