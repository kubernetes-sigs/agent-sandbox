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

from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from k8s_agent_sandbox_mcp_server.server import create_mcp_server


async def probe(settings, sandbox_client, path, run_lifespan=True):
    """GET path against a fresh app, optionally skipping the lifespan.

    The patch must stay active while the lifespan runs: the client is
    constructed inside it, not at create_mcp_server() time. Patching only
    around construction lets the real AsyncSandboxClient be built, which then
    talks to whatever cluster the ambient kubeconfig points at -- these tests
    hit a live apiserver and got a 401 before this was fixed.
    """
    with patch(
        "k8s_agent_sandbox_mcp_server.server.AsyncSandboxClient"
    ) as client_class:
        client_class.return_value = sandbox_client
        app = create_mcp_server(settings=settings).http_app()
        transport = httpx.ASGITransport(app=app)

        if not run_lifespan:
            async with httpx.AsyncClient(
                transport=transport, base_url="http://t"
            ) as c:
                return await c.get(path)

        async with app.router.lifespan_context(app):
            async with httpx.AsyncClient(
                transport=transport, base_url="http://t"
            ) as c:
                return await c.get(path)


@pytest.fixture
def k8s_client():
    client = AsyncMock()
    client.k8s_helper = MagicMock()
    client.k8s_helper.list_sandbox_claims = AsyncMock(return_value=[])
    return client


@pytest.mark.anyio
async def test_healthz_is_ok(mcp_server_settings, k8s_client):
    response = await probe(mcp_server_settings, k8s_client, "/healthz")

    assert response.status_code == 200
    assert response.text == "ok"


@pytest.mark.anyio
async def test_healthz_does_not_touch_kubernetes(mcp_server_settings, k8s_client):
    """Liveness must not depend on the apiserver.

    Wiring it to an external dependency would turn a control-plane blip into a
    rolling restart of every replica.
    """
    response = await probe(mcp_server_settings, k8s_client, "/healthz")

    assert response.status_code == 200
    k8s_client.k8s_helper.list_sandbox_claims.assert_not_awaited()


@pytest.mark.anyio
async def test_readyz_is_ready_when_kubernetes_reachable(
    mcp_server_settings, k8s_client
):
    response = await probe(mcp_server_settings, k8s_client, "/readyz")

    assert response.status_code == 200
    assert response.json() == {"ready": True}
    k8s_client.k8s_helper.list_sandbox_claims.assert_awaited_once_with(
        mcp_server_settings.probe_namespace
    )


@pytest.mark.anyio
async def test_readyz_is_not_ready_when_kubernetes_unreachable(
    mcp_server_settings, k8s_client
):
    k8s_client.k8s_helper.list_sandbox_claims = AsyncMock(
        side_effect=RuntimeError("connection refused")
    )
    response = await probe(mcp_server_settings, k8s_client, "/readyz")

    assert response.status_code == 503
    assert response.json()["ready"] is False
    # The reason must not leak the underlying exception text to an
    # unauthenticated caller.
    assert "connection refused" not in response.text


@pytest.mark.anyio
async def test_readyz_reports_starting_before_lifespan_runs(
    mcp_server_settings, k8s_client
):
    response = await probe(
        mcp_server_settings, k8s_client, "/readyz", run_lifespan=False
    )

    assert response.status_code == 503
    assert response.json() == {"ready": False, "reason": "starting"}


@pytest.mark.anyio
async def test_readyz_uses_the_configured_probe_namespace(k8s_client):
    from k8s_agent_sandbox_mcp_server.settings import (
        DirectConnectionConfig,
        Settings,
    )

    settings = Settings(
        connection=DirectConnectionConfig(api_url="http://some-url"),
        probe_namespace="sandboxes",
    )
    response = await probe(settings, k8s_client, "/readyz")

    assert response.status_code == 200
    k8s_client.k8s_helper.list_sandbox_claims.assert_awaited_once_with("sandboxes")
