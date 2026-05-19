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

from typing import Any
from abc import (
    ABC,
    abstractmethod,
)
from unittest import mock

import pytest

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations.adapter import (
    sandbox_result_to_json,
    sandbox_error_to_json,
)


class SandboxTestBase(ABC):
    def setup_method(self):
        self.sandbox_settings_mock = mock.MagicMock()

        self.sandbox_client_mock = mock.MagicMock()
        self.sandbox_client_mock.__enter__.return_value = self.sandbox_client_mock
        self.sandbox_client_mock.write = mock.MagicMock()
        self.sandbox_settings_mock.create_client.return_value = self.sandbox_client_mock


class SandboxResultTest(SandboxTestBase):
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

    def test_success(self, result_success):
        self._set_execution_result(result_success)
        result = self._execute_in_sandbox()
        assert result == result_success

    def test_failure(self, result_failure):
        self._set_execution_result(result_failure)
        result = self._execute_in_sandbox()
        assert result == result_failure

    @abstractmethod
    def _execute_in_sandbox(self) -> Any:
        pass

    def test_sandbox_error(self):
        result_error = Exception("some error")

        self._set_execution_error(result_error)
        with pytest.raises(Exception) as excinfo:
            self._execute_in_sandbox()

        assert "some error" in str(excinfo.value)

    def _set_execution_result(self, result: ExecutionResult):
        self.sandbox_client_mock.run.return_value = result
        self.sandbox_client_mock.agent.return_value = result

    def _set_execution_error(self, error: Exception):
        self.sandbox_client_mock.run.side_effect = error
        self.sandbox_client_mock.agent.side_effect = error


class SandboxConvertedResultTest(SandboxResultTest):
    def test_success(self, result_success):
        self._set_execution_result(result_success)
        result = self._execute_in_sandbox()
        expected_result = self._convert_sandbox_result(result_success)
        assert result == expected_result

    def test_failure(self, result_failure):
        self._set_execution_result(result_failure)
        result = self._execute_in_sandbox()
        expected_result = self._convert_sandbox_result(result_failure)
        assert result == expected_result

    def test_sandbox_error(self):
        result_error = Exception("some error")
        self._set_execution_error(result_error)
        result = self._execute_in_sandbox()
        expected_result = self._convert_sandbox_error(result_error)
        assert result == expected_result

    @abstractmethod
    def _convert_sandbox_result(self, result: ExecutionResult) -> Any:
        pass

    @abstractmethod
    def _convert_sandbox_error(self, error: Exception) -> Any:
        pass


class SandboxJsonResultTest(SandboxConvertedResultTest):

    def _convert_sandbox_result(self, result: ExecutionResult) -> dict:
        return sandbox_result_to_json(result)

    def _convert_sandbox_error(self, error: Exception) -> dict:
        return sandbox_error_to_json(error)
