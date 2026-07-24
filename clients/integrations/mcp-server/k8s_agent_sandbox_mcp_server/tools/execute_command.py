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

from pydantic import (
    BaseModel,
    Field,
)
from fastmcp import Context

from ..utils import (
    get_sandbox,
    TOOL_DEFAULT_TIMEOUT,
    TOOL_MAX_TIMEOUT,
)



class ExecuteCommandOutputSchema(BaseModel):
    exit_code: int = Field(description="The exit code of the executed process.")
    stdout: str = Field(description="The stdout of the executed process.")
    stderr: str = Field(description="The stderr of the executed process.")


async def execute_command(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(
        description="Name of a target sandbox claim.",
    )],
    namespace: Annotated[str, Field(
        description="Kubernetes namespace with a target sandbox."
    )],
    command: Annotated[str, Field(
        description="Shell command to execute inside a sandbox.",
    )],
    timeout: Annotated[int, Field(
        description="Time in seconds to execute the command before the timeout error.",
        gt=0,
        lt=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,

) -> ExecuteCommandOutputSchema:
    """
    Execute command in a sandbox.
    """

    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        execution_result = await sandbox.commands.run(command, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to execute command in sandbox: {e}") from e

    return ExecuteCommandOutputSchema(
        exit_code=execution_result.exit_code,
        stdout=execution_result.stdout,
        stderr=execution_result.stderr,
    )

