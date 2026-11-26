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

import json
import re

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
        "stdout_total_chars": 11,
        "stderr_total_chars": 9,
        "stdout_truncated": False,
        "stderr_truncated": False,
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
        "stdout_total_chars": 11,
        "stderr_total_chars": 9,
        "stdout_truncated": False,
        "stderr_truncated": False,
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
    mock_sandbox,
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

    mock_sandbox.commands.run.assert_not_awaited()


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_execute_command_bounds_output_by_default(mcp_client, mock_sandbox):
    mock_sandbox.commands.run.return_value = ExecutionResult(
        exit_code=7,
        stdout="stdout head\n" + "x" * 40000 + "\nstdout tail",
        stderr="stderr head\n" + "y" * 40000 + "\nstderr tail",
    )

    result = await mcp_client.call_tool(
        "execute_command",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "command": "some command",
        },
    )

    content = result.structured_content
    assert len(content["stdout"]) + len(content["stderr"]) <= 30000
    assert content["stdout"].startswith("stdout head\n")
    assert content["stdout"].endswith("\nstdout tail")
    assert content["stderr"].startswith("stderr head\n")
    assert content["stderr"].endswith("\nstderr tail")
    assert content["stdout_truncated"] is True
    assert content["stderr_truncated"] is True
    assert content["exit_code"] == 7
    assert result.is_error is False
    mock_sandbox.commands.run.assert_awaited_once_with("some command", timeout=60)
    _assert_output_excerpt(mock_sandbox.commands.run.return_value.stdout, content, "stdout", 15000)
    _assert_output_excerpt(mock_sandbox.commands.run.return_value.stderr, content, "stderr", 15000)
    # FastMCP also emits a text representation; it must not leak the full output.
    assert [json.loads(block.text) for block in result.content if block.type == "text"] == [content]


def _assert_output_excerpt(original, content, stream, budget):
    text = content[stream]
    assert len(text) <= budget
    assert content[f"{stream}_total_chars"] == len(original)
    if len(original) <= budget:
        assert text == original
        assert content[f"{stream}_truncated"] is False
        return

    assert content[f"{stream}_truncated"] is True
    marker = re.search(r"\n\[\.\.\. (\d+) characters omitted \.\.\.\]\n", text)
    assert marker is not None
    head, tail = text[:marker.start()], text[marker.end():]
    assert head and tail
    assert original.startswith(head)
    assert original.endswith(tail)
    assert abs(len(head) - len(tail)) <= 1
    assert int(marker.group(1)) == len(original) - len(head) - len(tail)


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
@pytest.mark.parametrize(
    "stdout,stderr,budget,stdout_budget,stderr_budget",
    [
        ("", "", 128, 0, 0),
        ("a" * 128, "", 128, 128, 0),
        ("a" * 129, "", 128, 128, 0),
        ("a" * 100, "b" * 100, 128, 64, 64),
        ("a" * 1000, "err", 128, 125, 3),
        ("out", "b" * 1000, 128, 3, 125),
        ("a" * 1000, "b" * 1000, 129, 64, 65),
        ("a" * 300001, "", 300000, 300000, 0),
        ("开始🙂\n" + "中e\u0301\t\x00" * 100 + "\n结束🚀", "错误Ω" * 100, 129, 64, 65),
        ("a" * 100, "b" * 100, "128", 64, 64),
        ("a" * 20001, "", "20000", 20000, 0),
        ("a" * 300001, "", "300000", 300000, 0),
        ("a" * 100, "b" * 100, 128.0, 64, 64),
    ],
    ids=[
        "empty", "exact", "one-over", "shared", "short-stderr", "short-stdout", "odd", "maximum", "unicode",
        "numeric-string-minimum", "numeric-string", "numeric-string-maximum", "integral-float",
    ],
)
async def test_execute_command_custom_output_budget(
    mcp_client, mock_sandbox, stdout, stderr, budget, stdout_budget, stderr_budget,
):
    mock_sandbox.commands.run.return_value = ExecutionResult(
        exit_code=0, stdout=stdout, stderr=stderr,
    )

    result = await mcp_client.call_tool(
        "execute_command",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "command": "some command",
            "timeout": 20,
            "max_output_chars": budget,
        },
    )

    content = result.structured_content
    assert len(content["stdout"]) + len(content["stderr"]) <= stdout_budget + stderr_budget
    _assert_output_excerpt(stdout, content, "stdout", stdout_budget)
    _assert_output_excerpt(stderr, content, "stderr", stderr_budget)
    assert content["exit_code"] == 0
    assert result.is_error is False
    mock_sandbox.commands.run.assert_awaited_once_with("some command", timeout=20)


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
@pytest.mark.parametrize(
    "budget", [0, -1, 127, 300001, None, True, False, 128.5, "128.5", "not-a-number", "127", "300001"],
)
async def test_execute_command_rejects_invalid_budget(
    mcp_client, mock_sandbox_client, mock_sandbox, budget,
):
    with pytest.raises(ToolError, match="max_output_chars"):
        await mcp_client.call_tool(
            "execute_command",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "command": "some command",
                "max_output_chars": budget,
            },
        )

    mock_sandbox_client.list_all_sandboxes.assert_not_awaited()
    mock_sandbox_client.get_sandbox.assert_not_awaited()
    mock_sandbox.commands.run.assert_not_awaited()


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_execute_command_preserves_sdk_failure(mcp_client, mock_sandbox):
    mock_sandbox.commands.run.side_effect = RuntimeError("connection lost")

    with pytest.raises(ToolError, match="Failed to execute command in sandbox: connection lost"):
        await mcp_client.call_tool(
            "execute_command",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "command": "some command",
            },
        )

    mock_sandbox.commands.run.assert_awaited_once_with("some command", timeout=60)


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_execute_command_advertises_output_budget(mcp_client):
    tools = await mcp_client.list_tools()
    tool = next(tool for tool in tools if tool.name == "execute_command")
    budget = tool.inputSchema["properties"]["max_output_chars"]
    assert budget["type"] == "integer"
    assert budget["default"] == 30000
    assert budget["minimum"] == 128
    assert budget["maximum"] == 300000
    assert "max_output_chars" not in tool.inputSchema["required"]
    assert set(tool.outputSchema["required"]) == {
        "exit_code", "stdout", "stderr", "stdout_total_chars", "stderr_total_chars",
        "stdout_truncated", "stderr_truncated",
    }
