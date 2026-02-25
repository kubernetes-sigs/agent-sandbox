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

from agentic_sandbox.integrations.adk.code_executors.python_sandbox import (
    PythonSandboxCodeExecutor,
)
from agentic_sandbox.integrations.adk.code_executors.common import (
    sandbox_result_to_code_executor_result,
    sandbox_error_to_code_executor_error,
)

from test_utils.integrations.sandbox_tests_base import SandboxTestBase


class TestADKPythonSandboxTool(SandboxTestBase):

    def test_success(self, result_success):

        self._set_execution_result(result_success)
        result = self._execute_in_sandbox()
        expected_result = sandbox_result_to_code_executor_result(result_success)
        assert result == expected_result

    def test_failure(self, result_failure):

        self._set_execution_result(result_failure)
        result = self._execute_in_sandbox()
        expected_result = sandbox_result_to_code_executor_result(result_failure)
        assert result == expected_result

    def test_sandbox_error(self, result_error):

        self._set_execution_error(result_error)
        result = self._execute_in_sandbox()
        expected_result = sandbox_error_to_code_executor_error(result_error)
        assert result == expected_result

    def _execute_in_sandbox(self):
        executor = PythonSandboxCodeExecutor(self.sandbox_settings_mock)
        mock_invocation_context = mock.MagicMock()

        result = executor.execute_code(
            mock_invocation_context, CodeExecutionInput(code="some code")
        )
        return result
