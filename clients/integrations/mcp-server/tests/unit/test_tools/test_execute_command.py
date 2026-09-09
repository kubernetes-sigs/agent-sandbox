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
from k8s_agent_sandbox.models import ExecutionResult
from fastmcp.exceptions import ToolError


@pytest.fixture
def execution_result():
    return ExecutionResult(
        exit_code=0,
        stdout="some output",
        stderr="some logs",
    )


@pytest.mark.anyio
@pytest.mark.usefixtures(
    "mocked_servers_sandbox_client_class",
)
async def test_call_execute_command_tool_with_default_args(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
    execution_result,
):
    mock_sandbox.commands.run.return_value = execution_result

    result = await mcp_client.call_tool(
        "execute_command", 
        {
            "sandbox_claim_name": "my-claim", 
            "namespace": "my-namespace",
            "command": "some command",
        },
    )

    assert result.structured_content == {
        "exit_code": 0,
        "stdout": "some output",
        "stderr": "some logs",
    }
    assert result.is_error is False
    mock_sandbox_client.get_sandbox.assert_called_once_with(
        "my-claim",
        namespace="my-namespace",
    )
    mock_sandbox.commands.run.assert_called_once_with(
        "some command",
        timeout=60,
    )

@pytest.mark.anyio
@pytest.mark.usefixtures(
    "mocked_servers_sandbox_client_class",
)
async def test_call_execute_command_tool_with_non_default_args(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
    execution_result,
):
    mock_sandbox.commands.run.return_value = execution_result

    result = await mcp_client.call_tool(
        "execute_command", 
        {
            "sandbox_claim_name": "my-claim", 
            "namespace": "my-namespace",
            "command": "some command",
            "timeout": 20,
        },

    )

    assert result.structured_content == {
        "exit_code": 0,
        "stdout": "some output",
        "stderr": "some logs",
    }
    assert result.is_error is False
    mock_sandbox_client.get_sandbox.assert_called_once_with(
        "my-claim",
        namespace="my-namespace",
    )
    mock_sandbox.commands.run.assert_called_once_with(
        "some command",
        timeout=20,
    )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_session_id_not_found(
    mcp_client,
    mock_sandbox_client,
):
    mock_sandbox_client.list_all_sandboxes.return_value = []

    with pytest.raises(ToolError, match="claim 'my-claim' is not found"):
        await mcp_client.call_tool(
            "execute_command",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "command": "some command",
            },
        )

