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

import pytest

from agentic_sandbox.sandbox_client import ExecutionResult


class SandboxTestBase:
    def setup_method(self):
        self.sandbox_settings_mock = mock.MagicMock()

        self.sandbox_client_mock = mock.MagicMock()
        self.sandbox_client_mock.__enter__.return_value = self.sandbox_client_mock
        self.sandbox_client_mock.write = mock.MagicMock()
        self.sandbox_settings_mock.create_client.return_value = self.sandbox_client_mock

    @pytest.fixture
    def result_success(self):
        return ExecutionResult(
            stdout="some output",
            stderr="some logs",
            exit_code=0,
        )

    @pytest.fixture
    def result_failure(self):
        return ExecutionResult(
            stdout="some output",
            stderr="some logs",
            exit_code=0,
        )

    @pytest.fixture
    def result_error(self):
        return Exception("some error")

    def _execute_in_sandbox(self):
        raise NotImplementedError

    def _set_execution_result(self, result: ExecutionResult):
        self.sandbox_client_mock.run.return_value = result
        self.sandbox_client_mock.agent.return_value = result

    def _set_execution_error(self, error: Exception):
        self.sandbox_client_mock.run.side_effect = error
        self.sandbox_client_mock.agent.side_effect = error
