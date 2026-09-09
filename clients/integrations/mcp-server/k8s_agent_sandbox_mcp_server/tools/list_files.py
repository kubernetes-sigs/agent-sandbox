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


# Upper bound on how many entries a single list_files call returns.
#
# This bounds the *MCP response*, not sandbox-side memory: the SDK's
# files.list() has already parsed the HTTP body and built the full
# list[FileEntry] before this tool sees it, so truncating here cannot
# reduce peak allocation. What it does prevent is a directory with tens of
# thousands of entries being serialized into an LLM's context window,
# where it would exhaust the token budget and crowd out the conversation.
#
# Callers that need a complete listing of a very large directory should
# use execute_command with a targeted shell invocation instead.
DEFAULT_MAX_ENTRIES = 1000

# Ceiling for the caller-supplied max_entries, so a client cannot opt out
# of the bound entirely.
MAX_ENTRIES_LIMIT = 10000


class FileEntrySchema(BaseModel):
    name: str = Field(description="Name of the entry.")
    size: int = Field(description="Size of the entry in bytes.")
    type: Literal["file", "directory"] = Field(description="Whether the entry is a file or a directory.")
    mod_time: float = Field(description="Last modification time as a POSIX timestamp.")


class ListFilesOutputSchema(BaseModel):
    entries: List[FileEntrySchema] = Field(description="Entries found in the directory.")
    total_entries: int = Field(
        description="Total number of entries in the directory, before any truncation."
    )
    truncated: bool = Field(
        description="True when the directory holds more entries than were returned. "
                    "Narrow the path or use execute_command to inspect the rest."
    )


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
    max_entries: Annotated[int, Field(
        description="Maximum number of entries to return. When the directory holds more, "
                    "the response is truncated and 'truncated' is set to True.",
        gt=0,
        le=MAX_ENTRIES_LIMIT,
    )] = DEFAULT_MAX_ENTRIES,
) -> ListFilesOutputSchema:
    """
    List the contents of a directory in a sandbox.

    At most max_entries entries are returned (1000 by default). When the
    directory holds more, the response is truncated and 'truncated' is True
    while 'total_entries' reports the full count.
    """
    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        entries = await sandbox.files.list(path, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to list directory in sandbox: {e}") from e

    total_entries = len(entries)

    return ListFilesOutputSchema(
        entries=[
            FileEntrySchema(
                name=entry.name,
                size=entry.size,
                type=entry.type,
                mod_time=entry.mod_time,
            )
            for entry in entries[:max_entries]
        ],
        total_entries=total_entries,
        truncated=total_entries > max_entries,
    )
