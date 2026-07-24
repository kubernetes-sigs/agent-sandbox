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

import base64

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


class DownloadFileOutputSchema(BaseModel):
    content: str = Field(description="The content of a file.")
    bytes_read: int = Field(description="Bytes read from a file.")


async def download_file(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(description="Name of a target sandbox claim.")],
    namespace: Annotated[str, Field(description="Kubernetes namespace with a target sandbox.")],
    path: Annotated[str, Field(description="The download path.")],
    binary: Annotated[bool, Field(description="When True, the content of a file is returned as a base64-encoded binary blob.")] = False,
    timeout: Annotated[int, Field(
        description="Time in seconds to download the file until the timeout.",
        gt=0,
        lt=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,
) -> DownloadFileOutputSchema:
    """
    Download a file from a sandbox.
    """
    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)

    try:
        content = await sandbox.files.read(path, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to read file from sandbox: {e}") from e

    try:
        if binary:
            final_content = base64.b64encode(content).decode("ascii")
        else:
            final_content = content.decode("utf-8")
    except Exception as e:
        raise ValueError(f"Failed to decode file content: {e}") from e

    return DownloadFileOutputSchema(
        content=final_content,
        bytes_read=len(content),
    )

