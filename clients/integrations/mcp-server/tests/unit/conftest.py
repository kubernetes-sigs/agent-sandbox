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

import pytest
from fastmcp.client import Client

from k8s_agent_sandbox_mcp_server.settings import (
    Settings,
    DirectConnectionConfig,
)
from k8s_agent_sandbox_mcp_server.server import create_mcp_server

@pytest.fixture
def mcp_server_settings():
    settings = Settings(
        connection=DirectConnectionConfig(api_url="http://some-url")
    )

    return settings


@pytest.fixture
def mcp_server(mcp_server_settings):
    return create_mcp_server(settings=mcp_server_settings)


@pytest.fixture
async def mcp_client(mcp_server):
    async with Client(transport=mcp_server) as mcp_client:
        yield mcp_client


@pytest.fixture
def mock_sandbox():
    sandbox = AsyncMock()
    sandbox.claim_name = "my-claim"
    return sandbox

@pytest.fixture
def mock_sandbox_client(mock_sandbox):
    client = AsyncMock()
    client.create_sandbox.return_value = mock_sandbox
    client.get_sandbox.return_value = mock_sandbox
    client.list_all_sandboxes.return_value = [mock_sandbox.claim_name]
    return client
    
@pytest.fixture
def mocked_servers_sandbox_client_class(mock_sandbox_client):
    with patch("k8s_agent_sandbox_mcp_server.server.AsyncSandboxClient") as m:
        m.return_value = mock_sandbox_client
        yield

