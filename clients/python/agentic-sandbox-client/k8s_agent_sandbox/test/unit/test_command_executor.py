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

"""Tests for synchronous and asynchronous command execution."""

import sys
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

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
        mock_connector.is_sandboxd.return_value = False  # legacy HTTP execute path
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
    """Verify async command routing, results, and tracing."""

    @patch("k8s_agent_sandbox.commands.async_command_executor.trace")
    async def test_async_executor_logs_executable(self, mock_trace):
        mock_span = MagicMock()
        mock_span.is_recording.return_value = True
        mock_trace.get_current_span.return_value = mock_span

        mock_connector = MagicMock()
        mock_connector.is_sandboxd.return_value = False
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

    async def test_async_sandboxd_executor_uses_grpc(self):
        mock_connector = MagicMock()
        mock_connector.is_sandboxd.return_value = True
        mock_connector.connect = AsyncMock()
        mock_connector.grpc_channel = AsyncMock(return_value=MagicMock())
        mock_response = MagicMock(stdout=b"hello", stderr=b"", exit_code=0)
        mock_pb2 = SimpleNamespace(
            ProcessConfig=MagicMock(), ExecuteRequest=MagicMock()
        )
        mock_pb2_grpc = SimpleNamespace(ProcessServiceStub=MagicMock())
        mock_pb2_grpc.ProcessServiceStub.return_value.Execute = AsyncMock(
            return_value=mock_response
        )
        mock_grpc = SimpleNamespace(RpcError=Exception)

        with patch.dict(
            sys.modules,
            {
                "grpc": mock_grpc,
                "k8s_agent_sandbox.commands._process_stubs": SimpleNamespace(
                    process_pb2=mock_pb2,
                    process_pb2_grpc=mock_pb2_grpc,
                )
            },
        ):
            executor = AsyncCommandExecutor(
                mock_connector, MagicMock(), "sandbox-client"
            )
            result = await executor.run("echo hello", timeout=12)

        mock_connector.connect.assert_awaited_once()
        mock_pb2.ExecuteRequest.assert_called_once()
        self.assertEqual(result.stdout, "hello")
        self.assertEqual(result.exit_code, 0)


if __name__ == "__main__":
    unittest.main()
