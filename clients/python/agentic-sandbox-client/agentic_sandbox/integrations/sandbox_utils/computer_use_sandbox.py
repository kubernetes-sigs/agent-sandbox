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

from agentic_sandbox.sandbox_client import ExecutionResult

from .sandbox_settings import SandboxSettings
from .tools import (
    sandbox_result_to_json,
    sandbox_error_to_json,
)


def execute_computer_use_query_in_sandbox(
    sandbox_settings: SandboxSettings, query: str, timeout: int = 60
) -> ExecutionResult:
    """
    Executes query in a Computer Use sandbox.

    Args:
        sandbox_settings: Settings to create a sandbox.
        query: String with human readable query for the agent inside the Computer Use sandbox.
        timeout: Execution timeout.

    Returns: Sandbox ExecutionResult instance.
    """

    with sandbox_settings.create_client() as sandbox:
        result = sandbox.agent(query, timeout)

        return result


def execute_computer_use_query_tool_and_handle_errors(
    sandbox_settings: SandboxSettings,
    query: str,
    timeout: int = 60,
) -> dict:
    """
    Executes query in a ComputerUse sandbox.
    It returns results and errors that can be handled by a common agent tool.

    Args:
        sandbox_settings: Settings to create a sandbox.
        query: String with human readable query for the agent inside the Computer Use sandbox.
        timeout: Execution timeout.

    Returns: Sandbox ExecutionResult instance.
    """

    try:
        result = execute_computer_use_query_in_sandbox(sandbox_settings, query, timeout)
    except Exception as e:
        return sandbox_error_to_json(e)

    return sandbox_result_to_json(result)
