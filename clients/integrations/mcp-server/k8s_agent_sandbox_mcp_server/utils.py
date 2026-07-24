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

from fastmcp import Context
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient

from .settings import Settings


TOOL_DEFAULT_TIMEOUT = 60
TOOL_MAX_TIMEOUT = 600

async def get_sandbox(
    ctx: Context,
    sandbox_claim_name: str,
    namespace: str,
):

    client: AsyncSandboxClient = ctx.lifespan_context["client"]

    # TODO: Get labels directly from the sanbox when it is in SDK.
    label_selector = get_session_label_selector_from_context(ctx)
    found = set(await client.list_all_sandboxes(
        namespace=namespace,
        label_selector=label_selector,
    ))

    if sandbox_claim_name not in found:
        raise RuntimeError(f"Sandbox claim '{sandbox_claim_name}' is not found in namespace '{namespace}'.")

    sandbox = await client.get_sandbox(sandbox_claim_name, namespace=namespace)

    return sandbox

def get_session_id_from_context(ctx: Context) -> str:

    session_id = getattr(ctx, "session_id", None)

    if session_id is None:
        raise RuntimeError(
            "This server requires a transport that provides a session id (e.g. streamable HTTP); "
            "ctx.session_id is None."
        )

    return session_id


def get_session_label_selector_from_context(ctx: Context) -> str:
    settings: Settings = ctx.lifespan_context["settings"]
    session_id = get_session_id_from_context(ctx)

    label_selector = f"{settings.session_id_label_key}={session_id}"
    return label_selector
