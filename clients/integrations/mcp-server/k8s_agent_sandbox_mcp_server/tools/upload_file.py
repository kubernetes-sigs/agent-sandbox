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


class UploadFileOutputSchema(BaseModel):
    bytes_written: int = Field(description="Bytes written to a file.")


async def upload_file(
    ctx: Context,
    sandbox_claim_name: Annotated[str, Field(description="Name of a target sandbox claim.")],
    namespace: Annotated[str, Field(description="Kubernetes namespace with a target sandbox.")],
    path: Annotated[str, Field(description="The upload path.")],
    content: Annotated[str, Field(description="Content of the file.")],
    binary: Annotated[bool, Field(description="When True, the 'content' argument is expected to be a base64-encoded binary blob.")] = False,
    timeout: Annotated[int, Field(
        description="Time in seconds to upload the file until the timeout.",
        gt=0,
        lt=TOOL_MAX_TIMEOUT,
    )] = TOOL_DEFAULT_TIMEOUT,
) -> UploadFileOutputSchema:
    """
    Upload file to a sandbox.
    """

    sandbox = await get_sandbox(ctx, sandbox_claim_name, namespace)
    try:
        if binary:
            content_bytes = base64.b64decode(content)
        else:
            content_bytes = content.encode("utf-8")
    except Exception as e:
        raise ValueError(f"Failed to decode or encode content: {e}") from e

    try:
        await sandbox.files.write(path, content_bytes, timeout=timeout)
    except Exception as e:
        raise RuntimeError(f"Failed to write file to sandbox: {e}") from e

    return UploadFileOutputSchema(
        bytes_written=len(content_bytes)
    )

