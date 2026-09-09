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



@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_read_get_sandboxes_resource_with_default_args(
    mcp_client,
    mock_sandbox_client,
):

    claims = [
        "my-claim",
        "second-claim",
        "another-claim",
    ]

    mock_sandbox_client.list_all_sandboxes.return_value = claims
    result = await mcp_client.read_resource(
        "sandboxes://my-namespace",
    )

    assert {str(c.uri) for c in result} == {"sandboxes://my-namespace"}
    assert {c.text for c in result} == set(claims)

