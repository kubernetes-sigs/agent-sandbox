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

from agentic_sandbox.integrations.sandbox_utils.tools import (
    COMMON_COMPUTER_USE_TOOL_DOCSTRING_DESCRIPTION,
)
from agentic_sandbox.integrations.sandbox_utils.computer_use_sandbox import (
    execute_computer_use_query_tool_and_handle_errors,
)
from .tool import sandbox_tool


def create_computer_use_sandbox_tool(
    sandbox_settings, description=COMMON_COMPUTER_USE_TOOL_DOCSTRING_DESCRIPTION
):
    """
    Create Langchain tool that executes Computer Use queries inside the Agent Sandbox.

    Args:
        sandbox_settings: Settings to create a sandbox.
        description: Tool description.

    """
    return sandbox_tool(sandbox_settings, description)(execute_query_in_sandbox)


def execute_query_in_sandbox(code: str, **kwargs) -> dict:
    sandbox_settings = kwargs["sandbox"]
    return execute_computer_use_query_tool_and_handle_errors(sandbox_settings, code)
