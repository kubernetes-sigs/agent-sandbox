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

import threading
from datetime import timedelta
from typing import Any, cast
from k8s_agent_sandbox.commands._process_handle import ProcessHandle
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import ExecutionResult, LegacyExecuteRequest, PtySize
from k8s_agent_sandbox.trace_manager import trace, trace_span

# Extra time the client waits beyond command_timeout, so the response the
# runtime sends after killing a timed-out command still arrives.
_COMMAND_TIMEOUT_MARGIN_SECONDS = 10

def _extract_executable(command: str) -> str:
    if not command:
        return ""
    for field in command.split():
        # Skip leading inline environment variables (e.g., KEY=VALUE)
        if "=" in field:
            continue
        # Extract base executable name (strip directory paths)
        return field.split("/")[-1]
    return ""


def _check_process_options(
    env: dict[str, str] | None, cwd: str | None, sandboxd: bool
) -> None:
    """Rejects env/cwd the runtime cannot honor instead of silently dropping them."""
    if not (env or cwd):
        return
    if not sandboxd:
        raise NotImplementedError(
            "env and cwd are only supported by the sandboxd runtime; the "
            "legacy python-runtime ignores them"
        )
    for key in env or {}:
        # sandboxd joins these as KEY=value, so such a key would corrupt it.
        if not key or "=" in key:
            raise ValueError(
                f"invalid env key {key!r}: must be non-empty and contain no '='"
            )


def _process_config(
    process_pb2: Any, command: str, env: dict[str, str] | None, cwd: str | None
) -> Any:
    """Builds the sandboxd ProcessConfig, keeping the shell-string API."""
    return process_pb2.ProcessConfig(
        command=["/bin/sh", "-c", command], env_vars=env, cwd=cwd or None
    )


def _limited_execution_result(
    process_pb2: Any, response: Any, command_timeout: float
) -> ExecutionResult:
    """Maps an ExecuteWithLimits response onto the run() result."""
    stdout = response.stdout.decode("utf-8", errors="replace")
    stderr = response.stderr.decode("utf-8", errors="replace")
    if response.termination_reason != (
        process_pb2.EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED
    ):
        if response.stdout_truncated or response.stderr_truncated:
            # ExecutionResult cannot mark output as partial, so fail the same
            # way Execute does when deployment policy truncates output.
            raise RuntimeError(
                "sandboxd truncated the command output under its deployment policy"
            )
        return ExecutionResult(
            stdout=stdout, stderr=stderr, exit_code=response.exit_code
        )
    if stderr and not stderr.endswith("\n"):
        stderr += "\n"
    # sandboxd reports the killed command's real status (usually 137); the
    # legacy runtime reports a timeout as 124, like GNU timeout.
    return ExecutionResult(
        stdout=stdout,
        stderr=f"{stderr}Command timed out after {command_timeout:g} seconds",
        exit_code=124,
        timed_out=True,
    )


def _execute_request(
    command: str, timeout: float, command_timeout: float | None
) -> tuple[dict[str, Any], float]:
    """Builds the legacy /execute payload and the read timeout to send it with."""
    payload = LegacyExecuteRequest(
        command=command, timeout_seconds=command_timeout
    ).model_dump(exclude_none=True)
    if command_timeout is None:
        return payload, timeout
    return payload, max(timeout, command_timeout + _COMMAND_TIMEOUT_MARGIN_SECONDS)


class CommandExecutor:
    """
    Handles execution of commands within the sandbox.
    """
    def __init__(
        self, connector: SandboxConnector, tracer: Any, trace_service_name: str
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @trace_span("run")
    def run(
        self,
        command: str,
        timeout: int = 60,
        command_timeout: float | None = None,
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
    ) -> ExecutionResult:
        """Run a shell command and return its output and exit code.

        Args:
            command: The shell command to run in the sandbox.
            timeout: Seconds to wait for the sandbox to respond.
            command_timeout: Optional limit, in seconds, on how long the command
                itself may run. The sandbox kills the command when it is
                exceeded, and the read timeout is extended past it so the
                result still arrives. Both runtimes report this as an
                ExecutionResult with exit code 124, ``timed_out`` set, and the
                output written before the limit. Legacy runtimes that predate
                the field ignore the limit. sandboxd versions without
                ExecuteWithLimits (v1.0.9 and earlier) enforce it as the gRPC
                deadline instead and do not return that output. Through the
                sandbox router a legacy runtime request is also bounded by the
                router's ``--proxy-timeout`` (180s by default), so a longer
                ``command_timeout`` ends in a 502/504 from the router instead
                of a ``timed_out`` result.
            env: Extra environment variables for the command, overriding the
                sandbox's own. Only the sandboxd runtime supports it; the
                legacy runtime raises NotImplementedError.
            cwd: Working directory for the command, confined to the sandbox
                root. Only the sandboxd runtime supports it; the legacy
                runtime raises NotImplementedError.
        """
        sandboxd = self.connector.is_sandboxd()
        _check_process_options(env, cwd, sandboxd)
        span = trace.get_current_span()
        if span.is_recording():
            executable = _extract_executable(command)
            span.set_attribute("sandbox.command.executable", executable)

        if sandboxd:
            result = self._run_sandboxd(
                command, timeout, command_timeout, env=env, cwd=cwd)
            if span.is_recording():
                span.set_attribute("sandbox.exit_code", result.exit_code)
            return result

        payload, read_timeout = _execute_request(command, timeout, command_timeout)
        response = self.connector.send_request(
            "POST", "execute", json=payload, timeout=read_timeout)

        try:
            response_data = response.json()
        except ValueError as e:
            raise RuntimeError(f"Failed to decode JSON response from sandbox: {response.text}") from e
        try:
            result = ExecutionResult(**response_data)
        except Exception as e:
            raise RuntimeError(f"Server returned invalid execution result format: {response_data}") from e

        if span.is_recording():
            span.set_attribute("sandbox.exit_code", result.exit_code)
        return result

    @trace_span("start")
    def start(
        self,
        command: str,
        timeout: float = 60,
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        pty: PtySize | None = None,
    ) -> ProcessHandle:
        """Start a command and stream its output as it is produced.

        Unlike :meth:`run`, which buffers until the command exits, this
        returns a :class:`ProcessHandle` whose ``events()`` yields output as
        it arrives and whose ``signal()`` can cancel the running process.

        Args:
            command: The shell command to run in the sandbox.
            timeout: Seconds to wait for sandboxd to report the process
                started. It does not bound the process's own lifetime.
            env: Extra environment variables for the command, overriding the
                sandbox's own.
            cwd: Working directory for the command, confined to the sandbox
                root.
            pty: Allocate a pseudo-terminal of this size. stderr is then
                merged into stdout, so no stderr events are produced.

        Only the sandboxd runtime implements ProcessService.Start; the legacy
        python-runtime raises ``NotImplementedError``.
        """
        sandboxd = self.connector.is_sandboxd()
        if not sandboxd:
            raise NotImplementedError(
                "commands.start() requires the sandboxd runtime; the legacy "
                "python-runtime does not expose a streaming exec endpoint"
            )
        _check_process_options(env, cwd, sandboxd)
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute(
                "sandbox.command.executable", _extract_executable(command))

        try:
            import grpc
            from k8s_agent_sandbox.commands._process_stubs import (
                process_pb2,
                process_pb2_grpc,
            )
        except ImportError as e:
            raise ImportError(
                "the sandboxd runtime requires gRPC support; install the "
                "'grpc' extra: pip install k8s-agent-sandbox[grpc]"
            ) from e

        # Publish the selected sandboxd gRPC target before dialing, as
        # _run_sandboxd does: a direct Pod IP connection refreshes its target
        # for every command.
        self.connector.connect()
        channel = self.connector.grpc_channel()
        stub = process_pb2_grpc.ProcessServiceStub(channel)
        request = process_pb2.StartRequest(
            config=_process_config(process_pb2, command, env, cwd),
            pty=process_pb2.PTY(cols=pty.cols, rows=pty.rows) if pty else None,
        )
        # No gRPC deadline on the call itself: that would cap the process's
        # lifetime, not just the wait for Init. A timer cancels the stream if
        # Init does not arrive, and is cancelled as soon as it does.
        stream_call = stub.Start(request)
        start_timed_out = threading.Event()

        def _cancel_unstarted() -> None:
            start_timed_out.set()
            stream_call.cancel()

        timer = threading.Timer(timeout, _cancel_unstarted)
        timer.start()
        # A sync streaming call reports failures on the first read, not on the
        # call itself, so both are covered by one handler.
        try:
            return ProcessHandle.from_stream(stub, process_pb2, stream_call)
        except grpc.RpcError as e:
            if start_timed_out.is_set():
                raise TimeoutError(
                    f"sandboxd did not report the process started within "
                    f"{timeout}s"
                ) from e
            rpc_error = cast("grpc.Call", e)
            if rpc_error.code() == grpc.StatusCode.UNAVAILABLE:
                self.connector.invalidate_sandboxd_transport(channel)
            raise RuntimeError(
                f"sandboxd Start failed ({rpc_error.code()}): {rpc_error.details()}"
            ) from e
        finally:
            timer.cancel()

    def _run_sandboxd(
        self,
        command: str,
        timeout: float,
        command_timeout: float | None = None,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
    ) -> ExecutionResult:
        """Execute via sandboxd's gRPC ProcessService.

        The shell-string API is preserved by wrapping the command in
        "/bin/sh -c", matching what the legacy python-runtime did
        server-side. Generated stubs are imported lazily so the REST
        filesystem path works without grpcio installed.

        With command_timeout, an exceeded limit is returned as a timed-out
        ExecutionResult instead of raising.
        """
        try:
            import grpc
            from k8s_agent_sandbox.commands._process_stubs import (
                process_pb2,
                process_pb2_grpc,
            )
        except ImportError as e:
            raise ImportError(
                "the sandboxd runtime requires gRPC support; install the "
                "'grpc' extra: pip install k8s-agent-sandbox[grpc]"
            ) from e

        # Publish the selected sandboxd gRPC target before dialing. A direct
        # Pod IP connection refreshes its target for every command.
        self.connector.connect()
        channel = self.connector.grpc_channel()
        stub = process_pb2_grpc.ProcessServiceStub(channel)
        config = _process_config(process_pb2, command, env, cwd)
        try:
            if command_timeout is not None:
                try:
                    response = stub.ExecuteWithLimits(
                        process_pb2.ExecuteWithLimitsRequest(
                            config=config,
                            time_limit=timedelta(seconds=command_timeout),
                        ),
                        timeout=command_timeout + _COMMAND_TIMEOUT_MARGIN_SECONDS,
                    )
                    return _limited_execution_result(
                        process_pb2, response, command_timeout)
                except grpc.RpcError as e:
                    # sandboxd rejects these before running the command: older
                    # versions lack the RPC, and a limit above the deployment
                    # maximum is invalid. Execute then runs it as before this
                    # RPC existed, without its output on timeout.
                    if cast("grpc.Call", e).code() not in (
                        grpc.StatusCode.UNIMPLEMENTED,
                        grpc.StatusCode.INVALID_ARGUMENT,
                    ):
                        raise
                timeout = command_timeout
            response = stub.Execute(
                process_pb2.ExecuteRequest(config=config), timeout=timeout)
        except grpc.RpcError as e:
            rpc_error = cast("grpc.Call", e)
            if (command_timeout is not None
                    and rpc_error.code() == grpc.StatusCode.DEADLINE_EXCEEDED):
                return ExecutionResult(
                    stderr=f"Command timed out after {command_timeout:g} seconds",
                    exit_code=124,
                    timed_out=True,
                )
            if rpc_error.code() == grpc.StatusCode.UNAVAILABLE:
                self.connector.invalidate_sandboxd_transport(channel)
            raise RuntimeError(
                f"sandboxd process service failed ({rpc_error.code()}): {rpc_error.details()}"
            ) from e
        return ExecutionResult(
            stdout=response.stdout.decode("utf-8", errors="replace"),
            stderr=response.stderr.decode("utf-8", errors="replace"),
            exit_code=response.exit_code,
        )
