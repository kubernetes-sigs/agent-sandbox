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

import pytest
from fastmcp.exceptions import ToolError
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.models import SandboxHealth

from k8s_agent_sandbox_mcp_server.utils import TOOL_MAX_TIMEOUT


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_health_tool(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    mock_sandbox.health.return_value = SandboxHealth(
        status="ok",
        uptime_seconds=3600,
    )

    result = await mcp_client.call_tool(
        "get_sandbox_health",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == {
        "healthy": True,
        "uptime_seconds": 3600,
        "message": "",
    }
    assert result.is_error is False
    mock_sandbox_client.get_sandbox.assert_called_once_with(
        "my-claim",
        namespace="my-namespace",
    )
    mock_sandbox.health.assert_called_once_with(timeout=60)


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_health_tool_with_non_default_args(
    mcp_client,
    mock_sandbox,
):
    mock_sandbox.health.return_value = SandboxHealth(
        status="ok",
        uptime_seconds=0,
    )

    result = await mcp_client.call_tool(
        "get_sandbox_health",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "timeout": 20,
        },
    )

    # A sandbox that just started reports zero uptime, not an error.
    assert result.structured_content == {
        "healthy": True,
        "uptime_seconds": 0,
        "message": "",
    }
    assert result.is_error is False
    mock_sandbox.health.assert_called_once_with(timeout=20)


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
@pytest.mark.parametrize("timeout", [0, TOOL_MAX_TIMEOUT + 1])
async def test_call_get_sandbox_health_tool_rejects_out_of_range_timeout(
    mcp_client,
    mock_sandbox,
    timeout,
):
    with pytest.raises(ToolError):
        await mcp_client.call_tool(
            "get_sandbox_health",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "timeout": timeout,
            },
        )

    mock_sandbox.health.assert_not_called()


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_health_tool_on_legacy_runtime(
    mcp_client,
    mock_sandbox,
):
    # The SDK raises NotImplementedError when the sandbox is not sandboxd. The
    # tool names that cause instead of reporting a generic failure.
    mock_sandbox.health.side_effect = NotImplementedError(
        "health() is only supported by the sandboxd runtime; the "
        "legacy python-runtime has no equivalent endpoint"
    )

    with pytest.raises(ToolError, match="requires the sandboxd runtime"):
        await mcp_client.call_tool(
            "get_sandbox_health",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
            },
        )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_health_tool_when_not_serving(
    mcp_client,
    mock_sandbox,
):
    # sandboxd serves 503 on /v1/health once it stops accepting work. The tool
    # reports that as a value, so an agent can tell a sandbox that is going
    # away apart from a command that failed on its own.
    mock_sandbox.health.side_effect = SandboxRequestError(
        "Failed to communicate with the sandbox.",
        status_code=503,
    )

    result = await mcp_client.call_tool(
        "get_sandbox_health",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
        },
    )

    assert result.is_error is False
    assert result.structured_content is not None
    assert result.structured_content["healthy"] is False
    assert result.structured_content["uptime_seconds"] is None
    assert "503" in result.structured_content["message"]


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_health_tool_surfaces_other_request_errors(
    mcp_client,
    mock_sandbox,
):
    # Only 503 means "not serving". Any other status says nothing about the
    # sandbox's health, so it must stay an error rather than read as unhealthy.
    mock_sandbox.health.side_effect = SandboxRequestError(
        "Failed to communicate with the sandbox.",
        status_code=401,
    )

    with pytest.raises(ToolError, match="Failed to get sandbox health"):
        await mcp_client.call_tool(
            "get_sandbox_health",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
            },
        )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_health_tool_surfaces_sandbox_failure(
    mcp_client,
    mock_sandbox,
):
    mock_sandbox.health.side_effect = RuntimeError("boom")

    with pytest.raises(ToolError, match="Failed to get sandbox health"):
        await mcp_client.call_tool(
            "get_sandbox_health",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
            },
        )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_session_id_not_found(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    mock_sandbox_client.list_all_sandboxes.return_value = []

    with pytest.raises(ToolError, match="claim 'my-claim' is not found"):
        await mcp_client.call_tool(
            "get_sandbox_health",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
            },
        )

    # The ownership check must fail closed: no sandbox call may happen.
    mock_sandbox.health.assert_not_called()
