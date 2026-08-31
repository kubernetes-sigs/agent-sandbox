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


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_status_tool_when_ready(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    mock_sandbox.status.return_value = ("SandboxReady", "Pod is running")

    result = await mcp_client.call_tool(
        "get_sandbox_status",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == {
        "status": "SandboxReady",
        "ready": True,
        "message": "Pod is running",
    }
    assert result.is_error is False
    mock_sandbox_client.get_sandbox.assert_called_once_with(
        "my-claim",
        namespace="my-namespace",
    )
    mock_sandbox.status.assert_called_once_with()


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_status_tool_when_not_ready(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    mock_sandbox.status.return_value = ("SandboxNotReady", "Pending scheduling")

    result = await mcp_client.call_tool(
        "get_sandbox_status",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == {
        "status": "SandboxNotReady",
        "ready": False,
        "message": "Pending scheduling",
    }
    assert result.is_error is False


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_status_tool_when_not_found(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    mock_sandbox.status.return_value = (
        "SandboxNotFound",
        "Sandbox object not found in Kubernetes.",
    )

    result = await mcp_client.call_tool(
        "get_sandbox_status",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == {
        "status": "SandboxNotFound",
        "ready": False,
        "message": "Sandbox object not found in Kubernetes.",
    }
    assert result.is_error is False


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_get_sandbox_status_tool_with_empty_condition_message(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    # A Kubernetes condition may carry an explicit null message.
    mock_sandbox.status.return_value = ("SandboxNotReady", None)

    result = await mcp_client.call_tool(
        "get_sandbox_status",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == {
        "status": "SandboxNotReady",
        "ready": False,
        "message": "",
    }
    assert result.is_error is False


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_sandbox_claim_not_found(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
):
    mock_sandbox_client.list_all_sandboxes.return_value = []

    with pytest.raises(ToolError, match="claim 'my-claim' is not found"):
        await mcp_client.call_tool(
            "get_sandbox_status",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
            },
        )

    mock_sandbox.status.assert_not_called()
