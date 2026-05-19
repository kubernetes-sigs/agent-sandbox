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


from pydantic import Field

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from .base import (
    SandboxIntegrationAdapter,
    CommonBaseInputSchema,
    CommonExecutionResultSchema,
)


class _InputSchema(CommonBaseInputSchema):
    code: str = Field(description="The code to execute.")


class PythonCodeSandboxIntegrationAdapter(SandboxIntegrationAdapter):
    """
    Sandbox Executor that executes Python code.
    """

    NAME = "execute_python_code_in_sandbox"
    DESCRIPTION = "Executes Python code in a sandbox and returns execution results."
    INPUT_SCHEMA = _InputSchema

    RESULT_SCHEMA = CommonExecutionResultSchema

    def _execute_code(self, code: str, timeout: int = 60) -> ExecutionResult:

        with self._sandbox_settings.create_client() as sandbox:
            sandbox.write("main.py", code)
            result = sandbox.run("python3 main.py", timeout)
            return result

    def execute(self, **args) -> ExecutionResult:
        return self._execute_code(**args)
