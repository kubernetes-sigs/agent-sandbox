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

import asyncio
from unittest import mock

from agentic_sandbox.integrations.sandbox_utils.tools import (
    sandbox_result_to_json,
    sandbox_error_to_json,
)
from agentic_sandbox.integrations.adk.tools.computer_use import ComputerUseSandboxTool
from test_utils.integrations.sandbox_tests_base import SandboxTestBase


class TestADKComputerUseSandboxTool(SandboxTestBase):

    def test_success(self, result_success):

        self._set_execution_result(result_success)
        result = self._execute_in_sandbox()
        expected_result = sandbox_result_to_json(result_success)
        assert result == expected_result

    def test_failure(self, result_failure):

        self._set_execution_result(result_failure)
        result = self._execute_in_sandbox()
        expected_result = sandbox_result_to_json(result_failure)
        assert result == expected_result

    def test_sandbox_error(self, result_error):

        self._set_execution_error(result_error)
        result = self._execute_in_sandbox()
        expected_result = sandbox_error_to_json(result_error)
        assert result == expected_result

    def _execute_in_sandbox(self):
        tool = ComputerUseSandboxTool(self.sandbox_settings_mock)
        result = asyncio.run(
            tool.run_async(args={"query": "some query"}, tool_context=mock.MagicMock())
        )
        return result
