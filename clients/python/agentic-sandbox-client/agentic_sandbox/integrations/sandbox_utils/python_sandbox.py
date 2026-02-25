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


def execute_python_code_in_sandbox(
    sandbox_settings: SandboxSettings, code: str
) -> ExecutionResult:
    """
    Executes Python code in a sandbox.
    Args:
        sandbox_settings: Settings to create a sandbox.

    Returns: Sandbox ExecutionResult instance.
    """

    with sandbox_settings.create_client() as sandbox:
        sandbox.write("main.py", code)

        result = sandbox.run("python3 main.py")

        return result


def execute_python_tool_and_handle_errors(
    sandbox_settings: SandboxSettings, code
) -> dict:
    """
    Executes Python code in a sandbox.
    It returns results and errors that can be handled by a common agent tool.
    Args:
        sandbox_settings: Settings to create a sandbox.

    Returns: Sandbox ExecutionResult instance.
    """

    try:
        result = execute_python_code_in_sandbox(sandbox_settings, code)
    except Exception as e:
        return sandbox_error_to_json(e)

    return sandbox_result_to_json(result)
