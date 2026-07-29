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

from contextlib import asynccontextmanager
import logging

from fastmcp import FastMCP

from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient

from .settings import Settings
from .tools import (
    create_sandbox,
    delete_sandbox,
    execute_command,
    upload_file,
    download_file,
)

from .resources import (
    get_sandboxes,
)


logger = logging.getLogger(__name__)



def create_mcp_server(settings: Settings | None = None):

    settings = settings or Settings()

    @asynccontextmanager
    async def lifespan(server: FastMCP):
     
        client = AsyncSandboxClient(
            connection_config=settings.connection,
            cleanup=False,
        )
        try:
            yield {"client": client, "settings": settings}
        finally:
            await client.close()

    mcp = FastMCP("K8sAgentSandbox", lifespan=lifespan)

    mcp.add_resource(get_sandboxes)
    
    mcp.add_tool(create_sandbox)
    mcp.add_tool(delete_sandbox)
    mcp.add_tool(execute_command)
    mcp.add_tool(upload_file)
    mcp.add_tool(download_file)

    return mcp

