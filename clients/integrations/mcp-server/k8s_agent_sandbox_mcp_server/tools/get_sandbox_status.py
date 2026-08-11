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

from typing import Annotated
from asyncio import CancelledError
from pydantic import (
    BaseModel,
    Field,
)
from fastmcp import Context

from ..utils import get_sandbox


class GetSandboxStatusOutputSchema(BaseModel):
    status: str = Field(
        description=(
            "Sandbox status string derived from Kubernetes conditions. One of "
            "'SandboxReady', 'SandboxNotReady', or 'SandboxNotFound'."
        )
    )
    ready: bool = Field(
        description="True only when the Sandbox 'Ready' condition is True.",
    )
    message: str = Field(
        description="Kubernetes condition message, if available.",
    )


async def get_sandbox_status(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(
        description="Name of a target sandbox claim.",
    )],
    namespace: Annotated[str, Field(
        description="Kubernetes namespace with a target sandbox.",
    )],
) -> GetSandboxStatusOutputSchema:
    """
    Get the readiness status of a sandbox.

    Use this before executing commands or transferring files to confirm the
    sandbox is Ready (e.g. after creation, resume, or warm-pool adoption).
    """

    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        status, message = await sandbox.status()
    except CancelledError:
        raise
    except Exception as e:
        raise RuntimeError(f"Failed to get sandbox status: {e}") from e

    return GetSandboxStatusOutputSchema(
        status=status,
        ready=(status == "SandboxReady"),
        message=message or "",
    )