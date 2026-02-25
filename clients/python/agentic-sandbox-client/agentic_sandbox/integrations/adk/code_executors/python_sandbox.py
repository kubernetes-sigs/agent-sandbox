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

from google.adk.agents.invocation_context import InvocationContext
from google.adk.code_executors.code_execution_utils import CodeExecutionInput
from google.adk.code_executors.code_execution_utils import CodeExecutionResult

from agentic_sandbox.integrations.sandbox_utils.python_sandbox import (
    execute_python_code_in_sandbox,
)
from .common import (
    SandboxCodeExecutor,
    sandbox_result_to_code_executor_result,
    sandbox_error_to_code_executor_error,
)


class PythonSandboxCodeExecutor(SandboxCodeExecutor):
    """
    An agent code executor that executes Python code in the Agent Sandbox

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    def execute_code(
        self,
        invocation_context: InvocationContext,
        code_execution_input: CodeExecutionInput,
    ) -> CodeExecutionResult:
        """
        Executes code in a sandbox.
        """

        try:
            result = execute_python_code_in_sandbox(
                self._sandbox_settings,
                code_execution_input.code,
            )
        except Exception as e:
            return sandbox_error_to_code_executor_error(e)

        return sandbox_result_to_code_executor_result(result)
