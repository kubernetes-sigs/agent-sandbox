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


class FileExistsOutputSchema(BaseModel):
    exists: bool = Field(description="Whether the path exists in the sandbox.")


async def file_exists(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(description="Name of a target sandbox claim.")],
    namespace: Annotated[str, Field(description="Kubernetes namespace with a target sandbox.")],
    path: Annotated[str, Field(description="The path to check.")],
    timeout: Annotated[int, Field(
        description="Time in seconds to check the path until the timeout.",
        gt=0,
        le=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,
) -> FileExistsOutputSchema:
    """
    Check whether a file or directory exists in a sandbox.
    """
    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        exists = await sandbox.files.exists(path, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to check path in sandbox: {e}") from e

    return FileExistsOutputSchema(exists=exists)
