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
from ..settings import Settings
from ..utils import get_session_id_from_context


class CreateSandboxOutputSchema(BaseModel):
    sandbox_claim_name: str = Field(description="Name of the created sandbox claim.")

async def create_sandbox(
    ctx: Context,
    warmpool: Annotated[str, Field(description="The name of the warmpool to use.")],
    namespace: Annotated[str, Field(description="The Kubernetes namespace in which to create the sandbox.")],
    sandbox_ready_timeout: Annotated[int, Field(description="Timeout in seconds to wait for the sandbox to be ready.")] = 180,
    labels: Annotated[dict[str, str] | None, Field(description="Additional labels for the sandbox.")] = None,
    shutdown_after_seconds: Annotated[int | None, Field(description="Time in seconds after which the sandbox automatically shuts down.")] = 300,
    pod_labels: Annotated[dict[str, str] | None, Field(description="Additional labels for the pod.")] = None,
    pod_annotations: Annotated[dict[str, str] | None, Field(description="Additional annotations for the pod.")] = None,
) -> CreateSandboxOutputSchema:
    """Create a new sandbox."""

    client = ctx.lifespan_context["client"]
    settings: Settings = ctx.lifespan_context["settings"]

    labels = labels or {}

    session_id = get_session_id_from_context(ctx)

    labels[settings.session_id_label_key] = session_id

    sandbox = await client.create_sandbox(
        warmpool,
        namespace=namespace,
        sandbox_ready_timeout=sandbox_ready_timeout,
        labels=labels,
        shutdown_after_seconds=shutdown_after_seconds,
        pod_labels=pod_labels,
        pod_annotations=pod_annotations,
    )

    return CreateSandboxOutputSchema(sandbox_claim_name=sandbox.claim_name)

