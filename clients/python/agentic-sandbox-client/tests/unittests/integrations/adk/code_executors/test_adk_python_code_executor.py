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

from unittest import mock

from google.adk.code_executors.code_execution_utils import (
    CodeExecutionInput,
)

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations.adk.code_executors.python_sandbox import (
    PythonADKSandboxCodeExecutor,
)
from k8s_agent_sandbox.integrations.adk.code_executors.base import (
    sandbox_result_to_code_executor_result,
    sandbox_error_to_code_executor_error,
)
from test_utils.integrations.sandbox_tests_base import SandboxConvertedResultTest


class TestADKPythonSandboxCodeExecutorResult(SandboxConvertedResultTest):

    def _execute_in_sandbox(self):
        executor = PythonADKSandboxCodeExecutor(
            sandbox_settings=self.sandbox_settings_mock
        )
        mock_invocation_context = mock.MagicMock()

        result = executor.execute_code(
            mock_invocation_context, CodeExecutionInput(code="some code")
        )
        return result

    def convert_sandbox_result(self, result: ExecutionResult):
        return sandbox_result_to_code_executor_result(result)

    def convert_sandbox_error(self, error: Exception):
        return sandbox_error_to_code_executor_error(error)
