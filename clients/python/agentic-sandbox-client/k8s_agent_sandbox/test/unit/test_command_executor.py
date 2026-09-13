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

import signal as sig
import sys
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

from k8s_agent_sandbox.commands.command_executor import CommandExecutor, _extract_executable
from k8s_agent_sandbox.commands.async_command_executor import AsyncCommandExecutor
from k8s_agent_sandbox.models import (
    ExecutionResult,
    ProcessExitEvent,
    ProcessOutputEvent,
    PtySize,
)



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


def _require_grpc():
    try:
        import grpc
        from k8s_agent_sandbox.commands._process_stubs import (
            process_pb2,
            process_pb2_grpc,
        )
    except ImportError:
        return None
    return process_pb2, process_pb2_grpc


def _make_start_response(
        pb2, *, init=None, stdout=None, stderr=None, exit_code=None):
    resp = pb2.StartResponse()
    if init is not None:
        resp.init.process_id = init
    elif stdout is not None:
        resp.stdout = stdout
    elif stderr is not None:
        resp.stderr = stderr
    elif exit_code is not None:
        resp.exit.exit_code = exit_code
    return resp


class _FakeStream:
    """Minimal server-stream stand-in yielding pre-built StartResponse messages."""

    def __init__(self, responses):
        self._responses = list(responses)
        self.cancelled = False

    def __iter__(self):
        return iter(self._responses)

    def cancel(self):
        self.cancelled = True


class TestProcessHandleSync(unittest.TestCase):
    """Exercise the streaming ProcessHandle using in-memory fakes.

    Skipped entirely when the grpc extra isn't installed, matching the
    treatment of the gRPC ProcessService path elsewhere in this suite.
    """

    def setUp(self):
        stubs = _require_grpc()
        if stubs is None:
            self.skipTest(
                "grpc extra not installed; sandboxd gRPC path is opt-in")
        self.pb2, self.pb2_grpc = stubs

    def _executor_with_stream(self, stream):
        stub = MagicMock()
        stub.Start.return_value = stream
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        connector.grpc_channel.return_value = MagicMock()
        executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
        # Patch the stub factory so no real channel is dialed.
        patcher = patch(
            "k8s_agent_sandbox.commands._process_stubs.process_pb2_grpc.ProcessServiceStub",
            return_value=stub,)
        patcher.start()
        self.addCleanup(patcher.stop)
        return executor, connector, stub

    def test_start_yields_output_then_exit(self):
        # Init is consumed by start() to populate process_id, so the public
        # event stream carries only output and the terminal exit. A handle
        # that restarted the stream would replay Init and raise here.
        stream = _FakeStream([
            _make_start_response(self.pb2, init=42),
            _make_start_response(self.pb2, stdout=b"hello"),
            _make_start_response(self.pb2, stderr=b"warn"),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, _, _ = self._executor_with_stream(stream)

        with executor.start("echo hello") as handle:
            events = list(handle.events())

        self.assertEqual(
            [(type(e), e.type) for e in events],
            [
                (ProcessOutputEvent, "stdout"),
                (ProcessOutputEvent, "stderr"),
                (ProcessExitEvent, "exit"),
            ],
        )
        self.assertEqual(events[0].data, b"hello")
        self.assertEqual(events[1].data, b"warn")
        self.assertEqual(events[2].exit_code, 0)

    def test_process_id_is_set_before_any_event_is_consumed(self):
        # Go's Start and TypeScript's start() both expose the pid immediately;
        # signalling must not require draining an event first.
        stream = _FakeStream([
            _make_start_response(self.pb2, init=42),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, _, _ = self._executor_with_stream(stream)

        with executor.start("sleep 5") as handle:
            self.assertEqual(handle.process_id, 42)

    def test_start_passes_env_cwd_and_pty(self):
        stream = _FakeStream([
            _make_start_response(self.pb2, init=1),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, _, stub = self._executor_with_stream(stream)

        with executor.start(
                "env", env={"FOO": "BAR"}, cwd="/workspace",
                pty=PtySize(cols=120, rows=40)):
            pass

        request = stub.Start.call_args.args[0]
        self.assertEqual(
            list(request.config.command), ["/bin/sh", "-c", "env"])
        self.assertEqual(dict(request.config.env_vars), {"FOO": "BAR"})
        self.assertEqual(request.config.cwd, "/workspace")
        self.assertEqual((request.pty.cols, request.pty.rows), (120, 40))

    def test_signal_sends_mapped_proto_signal(self):
        for sent, expected in ((sig.SIGINT, "SIGNAL_SIGINT"),
                               ("SIGTERM", "SIGNAL_SIGTERM")):
            with self.subTest(signal=sent):
                stream = _FakeStream([
                    _make_start_response(self.pb2, init=7),
                    _make_start_response(self.pb2, exit_code=0),
                ])
                executor, _, stub = self._executor_with_stream(stream)

                with executor.start("sleep 5") as handle:
                    handle.signal(sent)
                    list(handle.events())

                request = stub.SendSignal.call_args.args[0]
                self.assertEqual(request.process_id, 7)
                self.assertEqual(request.signal, getattr(self.pb2, expected))

    def test_signal_maps_grpc_errors_to_exception_taxonomy(self):
        import grpc

        for code, expected in (
            (grpc.StatusCode.NOT_FOUND, LookupError),
            (grpc.StatusCode.PERMISSION_DENIED, PermissionError),
        ):
            with self.subTest(code=code):
                stream = _FakeStream([_make_start_response(self.pb2, init=99)])
                executor, _, stub = self._executor_with_stream(stream)
                rpc_error = grpc.RpcError()
                rpc_error.code = lambda c=code: c
                rpc_error.details = lambda: "boom"
                stub.SendSignal.side_effect = rpc_error

                with executor.start("noop") as handle:
                    with self.assertRaises(expected):
                        handle.signal(sig.SIGTERM)

    def test_protocol_violations_are_rejected(self):
        # Mirrors the Go SDK's TestProcessHandle_ProtocolAndStableTerminal and
        # TestCommandsStart_InitializationFailures: a malformed stream must
        # fail loudly rather than look like a clean finish.
        init = lambda: _make_start_response(self.pb2, init=1)
        out = lambda: _make_start_response(self.pb2, stdout=b"late")
        exit_ = lambda: _make_start_response(self.pb2, exit_code=0)
        empty = lambda: self.pb2.StartResponse()

        cases = (
            ("output after exit", [init(), exit_(), out()]),
            ("duplicate exit", [init(), exit_(), exit_()]),
            ("repeated init", [init(), init()]),
            ("missing exit", [init(), out()]),
            ("unrecognized event", [init(), empty()]),
            # start() itself rejects a stream that does not open with Init.
            ("missing init", [out()]),
            ("init with zero pid", [_make_start_response(self.pb2, init=0)]),
        )
        for name, responses in cases:
            with self.subTest(case=name):
                executor, _, _ = self._executor_with_stream(
                    _FakeStream(responses))
                with self.assertRaises(RuntimeError):
                    with executor.start("noop") as handle:
                        list(handle.events())

    def test_start_rejects_legacy_connector(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        executor = CommandExecutor(connector, MagicMock(), "sandbox-client")
        with self.assertRaises(NotImplementedError):
            executor.start("echo hi")

    def test_context_manager_cancels_open_stream(self):
        stream = _FakeStream([_make_start_response(self.pb2, init=1)])
        executor, _, _ = self._executor_with_stream(stream)

        with executor.start("noop"):
            pass  # no exit event seen, so __exit__ must cancel
        self.assertTrue(stream.cancelled)


class _FakeAsyncStream:
    """Async server-stream stand-in yielding pre-built StartResponse messages."""

    def __init__(self, responses):
        # read() and `async for` share one position, as a grpc.aio streaming
        # call does: start() reads Init, then events() resumes after it.
        self._it = iter(list(responses))
        self.cancelled = False

    async def read(self):
        return next(self._it, None)

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return next(self._it)
        except StopIteration:
            raise StopAsyncIteration

    def cancel(self):
        self.cancelled = True



class TestAsyncProcessHandle(unittest.IsolatedAsyncioTestCase):
    """Mirrors TestProcessHandleSync so sync/async behavior stays in lock-step."""

    def setUp(self):
        stubs = _require_grpc()
        if stubs is None:
            self.skipTest(
                "grpc extra not installed; sandboxd gRPC path is opt-in")
        self.pb2, self.pb2_grpc = stubs

    def _executor_with_stream(self, stream, *, send_signal=None):
        stub = MagicMock()
        stub.Start = MagicMock(return_value=stream)
        if send_signal is None:
            async def _default_send_signal(_req):
                return None
            send_signal = _default_send_signal
        stub.SendSignal = send_signal

        async def _grpc_channel():
            return MagicMock()

        connector = MagicMock()
        connector.is_sandboxd = MagicMock(return_value=True)
        connector.grpc_channel = _grpc_channel
        # start() publishes the sandboxd target before dialing, as
        # _run_sandboxd does; the connector's connect() is awaited.
        connector.connect = AsyncMock(return_value=None)
        connector.invalidate_sandboxd_transport = AsyncMock(return_value=None)

        patcher = patch(
            "k8s_agent_sandbox.commands._process_stubs.process_pb2_grpc.ProcessServiceStub",
            return_value=stub,
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
        return executor, connector, stub

    async def test_start_yields_output_then_exit(self):
        stream = _FakeAsyncStream([
            _make_start_response(self.pb2, init=42),
            _make_start_response(self.pb2, stdout=b"hello"),
            _make_start_response(self.pb2, stderr=b"warn"),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, connector, _ = self._executor_with_stream(stream)

        async with await executor.start("echo hello") as handle:
            events = [e async for e in handle.events()]

        self.assertEqual(
            [(type(e), e.type) for e in events],
            [
                (ProcessOutputEvent, "stdout"),
                (ProcessOutputEvent, "stderr"),
                (ProcessExitEvent, "exit"),
            ],
        )
        self.assertEqual(events[0].data, b"hello")
        self.assertEqual(events[2].exit_code, 0)
        # start() must publish the sandboxd target before dialing, as
        # _run_sandboxd does, or a direct Pod IP connection dials a stale one.
        connector.connect.assert_awaited_once()

    async def test_process_id_is_set_before_any_event_is_consumed(self):
        stream = _FakeAsyncStream([
            _make_start_response(self.pb2, init=42),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, _, _ = self._executor_with_stream(stream)

        async with await executor.start("sleep 5") as handle:
            self.assertEqual(handle.process_id, 42)

    async def test_start_passes_env_cwd_and_pty(self):
        stream = _FakeAsyncStream([
            _make_start_response(self.pb2, init=1),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, _, stub = self._executor_with_stream(stream)

        async with await executor.start(
                "env", env={"FOO": "BAR"}, cwd="/workspace",
                pty=PtySize(cols=120, rows=40)):
            pass

        request = stub.Start.call_args.args[0]
        self.assertEqual(
            list(request.config.command), ["/bin/sh", "-c", "env"])
        self.assertEqual(dict(request.config.env_vars), {"FOO": "BAR"})
        self.assertEqual(request.config.cwd, "/workspace")
        self.assertEqual((request.pty.cols, request.pty.rows), (120, 40))

    async def test_signal_sends_mapped_proto_signal(self):
        captured = []

        async def _send_signal(request):
            captured.append(request)
            return None

        stream = _FakeAsyncStream([
            _make_start_response(self.pb2, init=7),
            _make_start_response(self.pb2, exit_code=0),
        ])
        executor, _, _ = self._executor_with_stream(
            stream, send_signal=_send_signal)

        async with await executor.start("sleep 5") as handle:
            await handle.signal(sig.SIGINT)
            [e async for e in handle.events()]

        self.assertEqual(captured[0].process_id, 7)
        self.assertEqual(captured[0].signal, self.pb2.SIGNAL_SIGINT)

    async def test_protocol_violations_are_rejected(self):
        # _to_event is shared with the sync path, which covers the full table;
        # the end-of-stream check is written separately in each events(), so
        # the async copy needs its own case.
        for name, responses in (
            ("missing exit", [_make_start_response(self.pb2, init=1),
                              _make_start_response(self.pb2, stdout=b"late")]),
            ("output after exit", [_make_start_response(self.pb2, init=1),
                                   _make_start_response(self.pb2, exit_code=0),
                                   _make_start_response(self.pb2, stdout=b"late")]),
        ):
            with self.subTest(case=name):
                executor, _, _ = self._executor_with_stream(
                    _FakeAsyncStream(responses))
                with self.assertRaises(RuntimeError):
                    async with await executor.start("noop") as handle:
                        [e async for e in handle.events()]

    async def test_start_rejects_legacy_connector(self):
        connector = MagicMock()
        connector.is_sandboxd = MagicMock(return_value=False)
        executor = AsyncCommandExecutor(connector, MagicMock(), "sandbox-client")
        with self.assertRaises(NotImplementedError):
            await executor.start("echo hi")

    async def test_context_manager_cancels_open_stream(self):
        stream = _FakeAsyncStream([_make_start_response(self.pb2, init=1)])
        executor, _, _ = self._executor_with_stream(stream)

        async with await executor.start("noop"):
            pass  # no exit event seen, so __aexit__ must cancel
        self.assertTrue(stream.cancelled)


if __name__ == "__main__":
    unittest.main()
