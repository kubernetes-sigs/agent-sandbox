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



DEFAULT_MAX_OUTPUT_CHARS = 30000
MAX_OUTPUT_CHARS_LIMIT = 300000
# Reserve space for an omission marker in both streams, even with a shared budget.
MIN_OUTPUT_CHARS = 128


def _truncate_output(output: str, max_chars: int) -> str:
    if len(output) <= max_chars:
        return output

    # The original length is an upper bound on the omitted count, so reserving
    # its marker leaves enough space even when that count gains a digit.
    marker = f"\n[... {len(output)} characters omitted ...]\n"
    retained_chars = max_chars - len(marker)
    head_chars = retained_chars // 2
    tail_chars = retained_chars - head_chars
    omitted_chars = len(output) - retained_chars
    return (
        output[:head_chars]
        + f"\n[... {omitted_chars} characters omitted ...]\n"
        + output[-tail_chars:]
    )


class ExecuteCommandOutputSchema(BaseModel):
    exit_code: int = Field(description="The exit code of the executed process.")
    stdout: str = Field(description="The stdout of the executed process, possibly truncated with an omission marker.")
    stderr: str = Field(description="The stderr of the executed process, possibly truncated with an omission marker.")
    stdout_total_chars: int = Field(description="Unicode character count of stdout received from the SDK before truncation.")
    stderr_total_chars: int = Field(description="Unicode character count of stderr received from the SDK before truncation.")
    stdout_truncated: bool = Field(description="Whether this tool omitted part of stdout.")
    stderr_truncated: bool = Field(description="Whether this tool omitted part of stderr.")


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
        le=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,
    max_output_chars: Annotated[int, Field(
        description="Maximum combined Unicode character count of returned stdout and stderr, "
                    "including omission markers. Oversized streams retain their beginning "
                    "and end. This limits returned text, not execution memory or tokens.",
        ge=MIN_OUTPUT_CHARS,
        le=MAX_OUTPUT_CHARS_LIMIT,
        strict=True,
    )] = DEFAULT_MAX_OUTPUT_CHARS,

) -> ExecuteCommandOutputSchema:
    """
    Execute command in a sandbox.
    """

    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        execution_result = await sandbox.commands.run(command, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to execute command in sandbox: {e}") from e

    stdout = execution_result.stdout
    stderr = execution_result.stderr
    # Give both streams a share so verbose stdout cannot hide stderr. Reuse
    # capacity left by either short stream; stderr gets the odd character.
    stdout_budget = min(len(stdout), max_output_chars // 2)
    stderr_budget = min(len(stderr), max_output_chars - stdout_budget)
    stdout_budget = min(len(stdout), max_output_chars - stderr_budget)

    return ExecuteCommandOutputSchema(
        exit_code=execution_result.exit_code,
        stdout=_truncate_output(stdout, stdout_budget),
        stderr=_truncate_output(stderr, stderr_budget),
        stdout_total_chars=len(stdout),
        stderr_total_chars=len(stderr),
        stdout_truncated=len(stdout) > stdout_budget,
        stderr_truncated=len(stderr) > stderr_budget,
    )

