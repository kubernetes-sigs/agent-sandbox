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

from typing import Annotated, List, Literal

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


class FileEntrySchema(BaseModel):
    name: str = Field(description="Name of the entry.")
    size: int = Field(description="Size of the entry in bytes.")
    type: Literal["file", "directory"] = Field(description="Whether the entry is a file or a directory.")
    mod_time: float = Field(description="Last modification time as a POSIX timestamp.")


class ListFilesOutputSchema(BaseModel):
    entries: List[FileEntrySchema] = Field(description="Entries found in the directory.")


async def list_files(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(description="Name of a target sandbox claim.")],
    namespace: Annotated[str, Field(description="Kubernetes namespace with a target sandbox.")],
    path: Annotated[str, Field(description="The directory path to list.")],
    timeout: Annotated[int, Field(
        description="Time in seconds to list the directory until the timeout.",
        gt=0,
        le=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,
) -> ListFilesOutputSchema:
    """
    List the contents of a directory in a sandbox.
    """
    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        entries = await sandbox.files.list(path, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to list directory in sandbox: {e}") from e

    return ListFilesOutputSchema(
        entries=[
            FileEntrySchema(
                name=entry.name,
                size=entry.size,
                type=entry.type,
                mod_time=entry.mod_time,
            )
            for entry in entries
        ],
    )
