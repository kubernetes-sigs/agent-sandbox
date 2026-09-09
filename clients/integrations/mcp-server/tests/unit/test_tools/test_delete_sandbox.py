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
async def test_call_delete_sandbox_tool_with_default_args(
    mcp_client,
    mock_sandbox_client,
):

    result = await mcp_client.call_tool(
        "delete_sandbox", 
        {
            "sandbox_claim_name": "my-claim", 
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == None
    assert result.is_error is False

    mock_sandbox_client.delete_sandbox.assert_called_once_with(
        "my-claim",
        namespace='my-namespace',
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
            "delete_sandbox",
            {
                "sandbox_claim_name": "my-claim",
                "namespace": "my-namespace",
            },
        )

