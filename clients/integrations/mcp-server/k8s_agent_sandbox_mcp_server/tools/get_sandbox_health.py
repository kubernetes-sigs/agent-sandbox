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
from k8s_agent_sandbox.exceptions import SandboxRequestError

from ..utils import (
    get_sandbox,
    TOOL_DEFAULT_TIMEOUT,
    TOOL_MAX_TIMEOUT,
)


# sandboxd serves 503 on /v1/health once it stops accepting work. A router or
# gateway in front of the Pod can serve 503 as well, so the tool reports that
# the sandbox is not serving instead of naming shutdown as the cause.
_NOT_SERVING_STATUS_CODE = 503

_NOT_SERVING_MESSAGE = (
    "sandboxd is not serving requests (HTTP 503). The sandbox is most likely "
    "shutting down."
)


class GetSandboxHealthOutputSchema(BaseModel):
    healthy: bool = Field(
        description="True when sandboxd is serving requests inside the sandbox.",
    )
    uptime_seconds: int | None = Field(
        default=None,
        ge=0,
        description=(
            "Seconds sandboxd has been running inside the sandbox. Null when "
            "the sandbox is not healthy."
        ),
    )
    message: str = Field(
        default="",
        description=(
            "Short reason the sandbox is not healthy. Empty when it is healthy."
        ),
    )


async def get_sandbox_health(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(
        description="Name of a target sandbox claim.",
    )],
    namespace: Annotated[str, Field(
        description="Kubernetes namespace with a target sandbox.",
    )],
    timeout: Annotated[int, Field(
        description="Time in seconds to query the sandbox until the timeout.",
        gt=0,
        le=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,
) -> GetSandboxHealthOutputSchema:
    """
    Report whether sandboxd is serving inside a sandbox.

    This complements get_sandbox_status, which reports Kubernetes readiness. A
    sandbox whose Pod is Ready can still report `healthy` false here, because
    sandboxd stops serving while it shuts down. Use this to tell a sandbox that
    is going away apart from a command that failed on its own, and so to decide
    whether to wait or create a new sandbox. Requires the sandboxd runtime.
    """

    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        health = await sandbox.health(timeout=timeout)
    except NotImplementedError as e:
        raise RuntimeError(
            f"get_sandbox_health requires the sandboxd runtime: {e}"
        ) from e
    except CancelledError:
        raise
    except SandboxRequestError as e:
        # A sandbox that stopped serving is a state the caller can act on, so
        # report it as a value. Every other request failure stays an error.
        if e.status_code != _NOT_SERVING_STATUS_CODE:
            raise RuntimeError(f"Failed to get sandbox health: {e}") from e
        return GetSandboxHealthOutputSchema(
            healthy=False,
            message=_NOT_SERVING_MESSAGE,
        )
    except Exception as e:
        raise RuntimeError(f"Failed to get sandbox health: {e}") from e

    return GetSandboxHealthOutputSchema(
        healthy=True,
        uptime_seconds=health.uptime_seconds,
    )
