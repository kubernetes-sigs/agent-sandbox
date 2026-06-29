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

import unittest
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.commands.command_executor import CommandExecutor, _extract_executable
from k8s_agent_sandbox.commands.async_command_executor import AsyncCommandExecutor
from k8s_agent_sandbox.models import ExecutionResult


class TestCommandExecutor(unittest.TestCase):

    def test_extract_executable(self):
        tests = [
            ("echo hello", "echo"),
            ("/usr/bin/python3 -c 'print()'", "python3"),
            ("API_KEY=secret_token TOKEN=xyz ./run.sh --arg", "run.sh"),
            ("  ", ""),
            ("", ""),
        ]
        for command, expected in tests:
            with self.subTest(command=command):
                self.assertEqual(_extract_executable(command), expected)

    @patch("k8s_agent_sandbox.commands.command_executor.trace")
    def test_sync_executor_logs_executable(self, mock_trace):
        mock_span = MagicMock()
        mock_span.is_recording.return_value = True
        mock_trace.get_current_span.return_value = mock_span

        mock_connector = MagicMock()
        mock_response = MagicMock()
        mock_response.json.return_value = {
            "stdout": "hello",
            "stderr": "",
            "exit_code": 0
        }
        mock_connector.send_request.return_value = mock_response

        executor = CommandExecutor(mock_connector, MagicMock(), "sandbox-client")
        result = executor.run("API_KEY=123 /usr/bin/python3 my_script.py")

        mock_span.set_attribute.assert_any_call("sandbox.command.executable", "python3")
        mock_span.set_attribute.assert_any_call("sandbox.exit_code", 0)
        self.assertEqual(result.stdout, "hello")


class TestAsyncCommandExecutor(unittest.IsolatedAsyncioTestCase):

    @patch("k8s_agent_sandbox.commands.async_command_executor.trace")
    async def test_async_executor_logs_executable(self, mock_trace):
        mock_span = MagicMock()
        mock_span.is_recording.return_value = True
        mock_trace.get_current_span.return_value = mock_span

        mock_connector = MagicMock()
        mock_response = MagicMock()
        mock_response.json.return_value = {
            "stdout": "hello_async",
            "stderr": "",
            "exit_code": 0
        }
        
        async def async_send(*args, **kwargs):
            return mock_response
        mock_connector.send_request = async_send

        executor = AsyncCommandExecutor(mock_connector, MagicMock(), "sandbox-client")
        result = await executor.run("API_KEY=123 /usr/bin/python3 my_script.py")

        mock_span.set_attribute.assert_any_call("sandbox.command.executable", "python3")
        mock_span.set_attribute.assert_any_call("sandbox.exit_code", 0)
        self.assertEqual(result.stdout, "hello_async")


if __name__ == "__main__":
    unittest.main()
