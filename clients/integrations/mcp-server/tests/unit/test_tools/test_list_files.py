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

from k8s_agent_sandbox_mcp_server.tools.list_files import MAX_ENTRIES_LIMIT
from k8s_agent_sandbox_mcp_server.utils import TOOL_MAX_TIMEOUT


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
        ],
        "total_entries": 2,
        "truncated": False,
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

    assert result.structured_content == {
        "entries": [],
        "total_entries": 0,
        "truncated": False,
    }
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

    assert result.structured_content == {
        "entries": [],
        "total_entries": 0,
        "truncated": False,
    }
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
async def test_call_list_files_tool_truncates_large_directory(
    mcp_client,
    mock_sandbox,
):
    mock_sandbox.files.list.return_value = [
        FileEntry(name=f"f{i}.txt", size=1, type="file", mod_time=0.0)
        for i in range(5)
    ]

    result = await mcp_client.call_tool(
        "list_files",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "path": "some/path",
            "max_entries": 2,
        },
    )

    content = result.structured_content
    assert [e["name"] for e in content["entries"]] == ["f0.txt", "f1.txt"]
    # total_entries reports the full directory, so the model is not misled
    # into thinking it saw everything.
    assert content["total_entries"] == 5
    assert content["truncated"] is True
    assert result.is_error is False


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_list_files_tool_not_truncated_at_exact_limit(
    mcp_client,
    mock_sandbox,
):
    mock_sandbox.files.list.return_value = [
        FileEntry(name=f"f{i}.txt", size=1, type="file", mod_time=0.0)
        for i in range(3)
    ]

    result = await mcp_client.call_tool(
        "list_files",
        {
            "sandbox_claim_name": "my-claim",
            "namespace": "my-namespace",
            "path": "some/path",
            "max_entries": 3,
        },
    )

    # Exactly at the limit is complete, not truncated.
    assert len(result.structured_content["entries"]) == 3
    assert result.structured_content["truncated"] is False
    assert result.is_error is False


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
@pytest.mark.parametrize("max_entries", [0, MAX_ENTRIES_LIMIT + 1])
async def test_call_list_files_tool_rejects_out_of_range_max_entries(
    mcp_client,
    mock_sandbox,
    max_entries,
):
    with pytest.raises(ToolError):
        await mcp_client.call_tool(
            "list_files",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "path": "some/path",
                "max_entries": max_entries,
            },
        )

    mock_sandbox.files.list.assert_not_called()


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
@pytest.mark.parametrize("timeout", [0, TOOL_MAX_TIMEOUT + 1])
async def test_call_list_files_tool_rejects_out_of_range_timeout(
    mcp_client,
    mock_sandbox,
    timeout,
):
    with pytest.raises(ToolError):
        await mcp_client.call_tool(
            "list_files",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
                "path": "some/path",
                "timeout": timeout,
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
