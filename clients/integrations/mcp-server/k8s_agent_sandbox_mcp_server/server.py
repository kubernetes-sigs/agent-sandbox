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
    list_files,
    file_exists,
    get_sandbox_status,
)

from .resources import (
    get_sandboxes,
)

from .probes import healthz, readyz


logger = logging.getLogger(__name__)



def create_mcp_server(settings: Settings | None = None):

    settings = settings or Settings()

    @asynccontextmanager
    async def lifespan(server: FastMCP):

        client = AsyncSandboxClient(
            connection_config=settings.connection,
            cleanup=False,
        )
        # The yielded mapping reaches tools via ctx.lifespan_context but is not
        # published on the ASGI app, so probe routes cannot read it. Stash what
        # they need on the server object, which routes reach as
        # request.app.state.fastmcp_server. Before this runs, /readyz correctly
        # reports "starting".
        server.sandbox_client = client
        server.probe_settings = settings
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
    mcp.add_tool(list_files)
    mcp.add_tool(file_exists)
    mcp.add_tool(get_sandbox_status)

    mcp.custom_route("/healthz", methods=["GET"])(healthz)
    mcp.custom_route("/readyz", methods=["GET"])(readyz)

    return mcp

