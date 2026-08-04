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

from k8s_agent_sandbox.models import FileEntry


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_list_files_tool_with_default_args(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox
):
    mock_sandbox.files.list.return_value = [
        FileEntry(name="a.txt", size=12, type="file", mod_time=1700000000.0),
        FileEntry(name="sub", size=4096, type="directory", mod_time=1700000001.5),
    ]

    result = await mcp_client.call_tool(
        "list_files",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "path": "some/path",
        },
    )

    assert result.structured_content == {
        "entries": [
            {
                "name": "a.txt",
                "size": 12,
                "type": "file",
                "mod_time": 1700000000.0,
            },
            {
                "name": "sub",
                "size": 4096,
                "type": "directory",
                "mod_time": 1700000001.5,
            },
        ]
    }
    assert result.is_error is False
    mock_sandbox_client.get_sandbox.assert_called_once_with(
        "my-claim",
        namespace="my-namespace",
    )
    mock_sandbox.files.list.assert_called_once_with(
        "some/path",
        timeout=60,
    )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_list_files_tool_with_non_default_args(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox
):
    mock_sandbox.files.list.return_value = []

    result = await mcp_client.call_tool(
        "list_files",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "path": "some/path",
            "timeout": 20,
        },
    )

    assert result.structured_content == {"entries": []}
    assert result.is_error is False
    mock_sandbox.files.list.assert_called_once_with(
        "some/path",
        timeout=20,
    )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_list_files_tool_on_empty_directory(
    mcp_client,
    mock_sandbox,
):
    mock_sandbox.files.list.return_value = []

    result = await mcp_client.call_tool(
        "list_files",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "path": "some/empty/path",
        },
    )

    assert result.structured_content == {"entries": []}
    assert result.is_error is False


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_list_files_tool_surfaces_sandbox_failure(
    mcp_client,
    mock_sandbox,
):
    mock_sandbox.files.list.side_effect = RuntimeError("boom")

    with pytest.raises(ToolError, match="Failed to list directory in sandbox"):
        await mcp_client.call_tool(
            "list_files",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "path": "some/path",
            },
        )


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_list_files_tool_rejects_out_of_range_timeout(
    mcp_client,
    mock_sandbox,
):
    with pytest.raises(ToolError):
        await mcp_client.call_tool(
            "list_files",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "path": "some/path",
                "timeout": 0,
            },
        )

    mock_sandbox.files.list.assert_not_called()


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
            "list_files",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "path": "some/path",
            },
        )

    # The ownership check must fail closed: no sandbox call may happen.
    mock_sandbox.files.list.assert_not_called()
