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

COMMON_TOOL_RESULT_DESCRIPTION = """
    Dictionary with the following fields:
      - status: The execution status with the next possible values:
          * success: Execution is successful.
          * failure: Execution completed but with failure.
          * error: Sandbox failed with unexpected error.
      - exit_code: Exit code of the executed process. Presented only when 'status' is not 'error'
      - stdout: Stdout of the executed code. Presented only when 'status' is not 'error'.
      - stderr: Stderr of the executed code. When 'status' field is 'error' is contains an error message.
      - sandbox_error: Optional. Text of the sandbox error.
"""


COMMON_CODE_TOOL_DESCRIPTION = (
    "Executes the code in a sandbox and returns execution results."
)
COMMON_CODE_TOOL_CODE_ARG_DESCRIPTION = "The code to execute."

COMMON_CODE_TOOL_DOCSTRING_DESCRIPTION = f"""
{COMMON_CODE_TOOL_DESCRIPTION}
Args:
    code: {COMMON_CODE_TOOL_CODE_ARG_DESCRIPTION}
Returns:
{COMMON_TOOL_RESULT_DESCRIPTION}
"""

COMMON_COMPUTER_USE_TOOL_DESCRIPTION = (
    "Executes the code in a sandbox and returns execution results."
)

COMMON_COMPUTER_USE_TOOL_QUERY_ARG_DESCRIPTION = (
    "String with a natural language query to execute within the sandbox."
)

COMMON_COMPUTER_USE_TOOL_DOCSTRING_DESCRIPTION = f"""
{COMMON_COMPUTER_USE_TOOL_DESCRIPTION}
Args:
    query: {COMMON_COMPUTER_USE_TOOL_QUERY_ARG_DESCRIPTION}
Returns:
{COMMON_TOOL_RESULT_DESCRIPTION}
"""


def sandbox_result_to_json(execution_result: ExecutionResult):
    """
    Convert sandbox result to a JSON serializable format
    that can be used by the most of the agent tools,
    """
    return {
        "status": ("success" if execution_result.exit_code == 0 else "failure"),
        "exit_code": execution_result.exit_code,
        "stdout": execution_result.stdout,
        "stderr": execution_result.stderr,
    }


def sandbox_error_to_json(error: Exception):
    """
    Return JSON serializable error message that
    can be used by the most of the agent tools.
    """
    return {
        "status": "error",
        "stderr": str(error),
    }
