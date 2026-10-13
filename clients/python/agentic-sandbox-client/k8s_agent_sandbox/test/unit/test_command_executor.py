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



class DeadlineRpcError(Exception):
    def code(self):
        return "DEADLINE_EXCEEDED"

    def details(self):
        return "Deadline Exceeded"


def _deadline_modules(stub):
    return {
        "grpc": SimpleNamespace(
            RpcError=DeadlineRpcError,
            StatusCode=SimpleNamespace(
                UNAVAILABLE="UNAVAILABLE", DEADLINE_EXCEEDED="DEADLINE_EXCEEDED"
            ),
        ),
        "k8s_agent_sandbox.commands._process_stubs": SimpleNamespace(
            process_pb2=SimpleNamespace(
                ProcessConfig=MagicMock(), ExecuteRequest=MagicMock()
            ),
            process_pb2_grpc=stub,
        ),
    }


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

    def _legacy_connector(self, response_data):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        connector.send_request.return_value.json.return_value = response_data
        return connector

    def test_sync_run_without_command_timeout_sends_command_only(self):
        connector = self._legacy_connector({"stdout": "", "stderr": "", "exit_code": 0})

        result = CommandExecutor(connector, MagicMock(), "sandbox-client").run(
            "echo hello", timeout=30
        )

        connector.send_request.assert_called_once_with(
            "POST", "execute", json={"command": "echo hello"}, timeout=30
        )
        self.assertFalse(result.timed_out)

    def test_sync_run_sends_command_timeout_and_outlasts_it(self):
        connector = self._legacy_connector({
            "stdout": "partial",
            "stderr": "Command timed out after 5 seconds",
            "exit_code": 124,
            "timed_out": True,
        })

        result = CommandExecutor(connector, MagicMock(), "sandbox-client").run(
            "sleep 60", command_timeout=5
        )

        args, kwargs = connector.send_request.call_args
        self.assertEqual(args, ("POST", "execute"))
        self.assertEqual(kwargs["json"], {"command": "sleep 60", "timeout_seconds": 5})
        # The default 60s read timeout already outlasts the 5s command limit.
        self.assertEqual(kwargs["timeout"], 60)
        self.assertTrue(result.timed_out)
        self.assertEqual(result.exit_code, 124)
        self.assertEqual(result.stdout, "partial")

    def test_sync_run_extends_read_timeout_past_command_timeout(self):
        connector = self._legacy_connector({"stdout": "", "stderr": "", "exit_code": 0})

        CommandExecutor(connector, MagicMock(), "sandbox-client").run(
            "make build", timeout=60, command_timeout=600
        )

        self.assertGreater(connector.send_request.call_args.kwargs["timeout"], 600)

    def test_sync_sandboxd_uses_command_timeout_as_deadline(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
        with patch.object(executor, "_run_sandboxd") as run_sandboxd:
            run_sandboxd.return_value = ExecutionResult(exit_code=0)
            executor.run("echo hello", timeout=60, command_timeout=5)

        run_sandboxd.assert_called_once_with(
            "echo hello", 5, report_timeout=True, env=None, cwd=None)

    def test_sync_sandboxd_unavailable_invalidates_without_replaying(self):
        class UnavailableRpcError(Exception):
            def code(self):
                return "UNAVAILABLE"

            def details(self):
                return "connection lost"

        connector = MagicMock()
        channel = MagicMock()
        connector.grpc_channel.return_value = channel
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute.side_effect = UnavailableRpcError()
        fake_grpc = SimpleNamespace(
            RpcError=UnavailableRpcError,
            StatusCode=SimpleNamespace(UNAVAILABLE="UNAVAILABLE"),
        )
        with patch.dict(
            sys.modules,
            {
                "grpc": fake_grpc,
                "k8s_agent_sandbox.commands._process_stubs": SimpleNamespace(
                    process_pb2=SimpleNamespace(
                        ProcessConfig=MagicMock(), ExecuteRequest=MagicMock()
                    ),
                    process_pb2_grpc=stub,
                ),
            },
        ):
            executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
            with self.assertRaisesRegex(RuntimeError, "connection lost"):
                executor._run_sandboxd("echo hello", timeout=12)
        connector.invalidate_sandboxd_transport.assert_called_once_with(channel)
        stub.ProcessServiceStub.return_value.Execute.assert_called_once()


    def test_sync_sandboxd_passes_env_and_cwd_to_process_config(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute.return_value = MagicMock(
            stdout=b"", stderr=b"", exit_code=0)
        process_pb2 = SimpleNamespace(
            ProcessConfig=MagicMock(), ExecuteRequest=MagicMock())
        with patch.dict(sys.modules, {
            "grpc": SimpleNamespace(RpcError=Exception),
            "k8s_agent_sandbox.commands._process_stubs": SimpleNamespace(
                process_pb2=process_pb2, process_pb2_grpc=stub),
        }):
            executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
            executor.run("pwd", env={"A": "1"}, cwd="work")

        process_pb2.ProcessConfig.assert_called_once_with(
            command=["/bin/sh", "-c", "pwd"], env_vars={"A": "1"}, cwd="work")

    def test_sync_legacy_runtime_rejects_env_and_cwd(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
        for kwargs in ({"env": {"A": "1"}}, {"cwd": "work"}):
            with self.subTest(kwargs=kwargs):
                with self.assertRaisesRegex(NotImplementedError, "sandboxd"):
                    executor.run("true", **kwargs)
        connector.send_request.assert_not_called()

    def test_sync_sandboxd_rejects_invalid_env_key(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
        for key in ("", "A=B"):
            with self.subTest(key=key):
                with self.assertRaisesRegex(ValueError, "invalid env key"):
                    executor.run("true", env={key: "1"})
        connector.connect.assert_not_called()

    def test_sync_sandboxd_deadline_with_command_timeout_reports_timed_out(self):
        connector = MagicMock()
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute.side_effect = DeadlineRpcError()
        with patch.dict(sys.modules, _deadline_modules(stub)):
            executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
            result = executor._run_sandboxd("sleep 30", timeout=2, report_timeout=True)

        self.assertTrue(result.timed_out)
        self.assertEqual(result.exit_code, 124)
        self.assertEqual(result.stderr, "Command timed out after 2 seconds")
        connector.invalidate_sandboxd_transport.assert_not_called()

    def test_sync_sandboxd_deadline_without_command_timeout_still_raises(self):
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute.side_effect = DeadlineRpcError()
        with patch.dict(sys.modules, _deadline_modules(stub)):
            executor = CommandExecutor(MagicMock(), MagicMock(), "sandbox-client")
            with self.assertRaisesRegex(RuntimeError, "Deadline Exceeded"):
                executor._run_sandboxd("sleep 30", timeout=2)


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

    async def test_async_run_without_command_timeout_sends_command_only(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        connector.send_request = AsyncMock(return_value=MagicMock())
        connector.send_request.return_value.json.return_value = {
            "stdout": "", "stderr": "", "exit_code": 0,
        }

        result = await AsyncCommandExecutor(connector, MagicMock(), "sandbox-client").run(
            "echo hello", timeout=30
        )

        connector.send_request.assert_awaited_once_with(
            "POST", "execute", json={"command": "echo hello"}, timeout=30
        )
        self.assertFalse(result.timed_out)

    async def test_async_run_sends_command_timeout_and_outlasts_it(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        connector.send_request = AsyncMock(return_value=MagicMock())
        connector.send_request.return_value.json.return_value = {
            "stdout": "partial",
            "stderr": "Command timed out after 600 seconds",
            "exit_code": 124,
            "timed_out": True,
        }

        result = await AsyncCommandExecutor(connector, MagicMock(), "sandbox-client").run(
            "make build", timeout=60, command_timeout=600
        )

        kwargs = connector.send_request.call_args.kwargs
        self.assertEqual(kwargs["json"], {"command": "make build", "timeout_seconds": 600})
        self.assertGreater(kwargs["timeout"], 600)
        self.assertTrue(result.timed_out)
        self.assertEqual(result.exit_code, 124)
        self.assertEqual(result.stdout, "partial")

    async def test_async_sandboxd_uses_command_timeout_as_deadline(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
        with patch.object(executor, "_run_sandboxd", new_callable=AsyncMock) as run_sandboxd:
            run_sandboxd.return_value = ExecutionResult(exit_code=0)
            await executor.run("echo hello", timeout=60, command_timeout=5)

        run_sandboxd.assert_awaited_once_with(
            "echo hello", 5, report_timeout=True, env=None, cwd=None)

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

    async def test_async_sandboxd_unavailable_invalidates_without_replaying(self):
        class UnavailableRpcError(Exception):
            def code(self):
                return "UNAVAILABLE"

            def details(self):
                return "connection lost"

        connector = MagicMock()
        connector.connect = AsyncMock()
        channel = MagicMock()
        connector.grpc_channel = AsyncMock(return_value=channel)
        connector.invalidate_sandboxd_transport = AsyncMock()
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute = AsyncMock(
            side_effect=UnavailableRpcError()
        )
        fake_grpc = SimpleNamespace(
            RpcError=UnavailableRpcError,
            StatusCode=SimpleNamespace(UNAVAILABLE="UNAVAILABLE"),
        )
        with patch.dict(
            sys.modules,
            {
                "grpc": fake_grpc,
                "k8s_agent_sandbox.commands._process_stubs": SimpleNamespace(
                    process_pb2=SimpleNamespace(
                        ProcessConfig=MagicMock(), ExecuteRequest=MagicMock()
                    ),
                    process_pb2_grpc=stub,
                ),
            },
        ):
            executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
            with self.assertRaisesRegex(RuntimeError, "connection lost"):
                await executor._run_sandboxd("echo hello", timeout=12)
        connector.invalidate_sandboxd_transport.assert_awaited_once_with(channel)
        stub.ProcessServiceStub.return_value.Execute.assert_awaited_once()

    async def test_async_sandboxd_passes_env_and_cwd_to_process_config(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        connector.connect = AsyncMock()
        connector.grpc_channel = AsyncMock(return_value=MagicMock())
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute = AsyncMock(
            return_value=MagicMock(stdout=b"", stderr=b"", exit_code=0))
        process_pb2 = SimpleNamespace(
            ProcessConfig=MagicMock(), ExecuteRequest=MagicMock())
        with patch.dict(sys.modules, {
            "grpc": SimpleNamespace(RpcError=Exception),
            "k8s_agent_sandbox.commands._process_stubs": SimpleNamespace(
                process_pb2=process_pb2, process_pb2_grpc=stub),
        }):
            executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
            await executor.run("pwd", env={"A": "1"}, cwd="work")

        process_pb2.ProcessConfig.assert_called_once_with(
            command=["/bin/sh", "-c", "pwd"], env_vars={"A": "1"}, cwd="work")

    async def test_async_legacy_runtime_rejects_env_and_cwd(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        connector.send_request = AsyncMock()
        executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
        for kwargs in ({"env": {"A": "1"}}, {"cwd": "work"}):
            with self.subTest(kwargs=kwargs):
                with self.assertRaisesRegex(NotImplementedError, "sandboxd"):
                    await executor.run("true", **kwargs)
        connector.send_request.assert_not_awaited()

    async def test_async_sandboxd_rejects_invalid_env_key(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        connector.connect = AsyncMock()
        executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
        for key in ("", "A=B"):
            with self.subTest(key=key):
                with self.assertRaisesRegex(ValueError, "invalid env key"):
                    await executor.run("true", env={key: "1"})
        connector.connect.assert_not_awaited()

    async def test_async_sandboxd_deadline_with_command_timeout_reports_timed_out(self):
        connector = MagicMock()
        connector.connect = AsyncMock()
        connector.grpc_channel = AsyncMock(return_value=MagicMock())
        connector.invalidate_sandboxd_transport = AsyncMock()
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute = AsyncMock(
            side_effect=DeadlineRpcError()
        )
        with patch.dict(sys.modules, _deadline_modules(stub)):
            executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
            result = await executor._run_sandboxd(
                "sleep 30", timeout=1.5, report_timeout=True
            )

        self.assertTrue(result.timed_out)
        self.assertEqual(result.exit_code, 124)
        self.assertEqual(result.stderr, "Command timed out after 1.5 seconds")
        connector.invalidate_sandboxd_transport.assert_not_awaited()

    async def test_async_sandboxd_deadline_without_command_timeout_still_raises(self):
        connector = MagicMock()
        connector.connect = AsyncMock()
        connector.grpc_channel = AsyncMock(return_value=MagicMock())
        stub = MagicMock()
        stub.ProcessServiceStub.return_value.Execute = AsyncMock(
            side_effect=DeadlineRpcError()
        )
        with patch.dict(sys.modules, _deadline_modules(stub)):
            executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
            with self.assertRaisesRegex(RuntimeError, "Deadline Exceeded"):
                await executor._run_sandboxd("sleep 30", timeout=1.5)


if __name__ == "__main__":
    unittest.main()
