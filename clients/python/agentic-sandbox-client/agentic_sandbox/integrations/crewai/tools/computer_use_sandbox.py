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

from typing import Type
from pydantic import BaseModel, Field

from agentic_sandbox.integrations.sandbox_utils.computer_use_sandbox import (
    execute_computer_use_query_tool_and_handle_errors,
)

from agentic_sandbox.integrations.sandbox_utils.tools import (
    COMMON_COMPUTER_USE_TOOL_DESCRIPTION,
    COMMON_COMPUTER_USE_TOOL_QUERY_ARG_DESCRIPTION,
    COMMON_TOOL_RESULT_DESCRIPTION,
)
from .base import CrewAISandboxTool


TOOL_DESCRIPTION = f"""
{COMMON_COMPUTER_USE_TOOL_DESCRIPTION}
Returns:
{COMMON_TOOL_RESULT_DESCRIPTION}
"""


class ComputerUseSandboxInput(BaseModel):
    """Input schema for Computer Use Sandbox Tool."""

    query: str = Field(..., description=COMMON_COMPUTER_USE_TOOL_QUERY_ARG_DESCRIPTION)


class ComputerUseSandboxTool(CrewAISandboxTool):
    name: str = "Computer use in Sandbox"
    description: str = TOOL_DESCRIPTION
    args_schema: Type[BaseModel] = ComputerUseSandboxInput

    def _run(self, query: str) -> dict:
        result = execute_computer_use_query_tool_and_handle_errors(
            self._sandbox_settings, query
        )
        return result
