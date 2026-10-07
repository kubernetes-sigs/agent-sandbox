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
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import ExecutionResult, LegacyExecuteRequest
from k8s_agent_sandbox.trace_manager import trace, trace_span

# Extra time the HTTP read waits beyond command_timeout, so the response the
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
        self, command: str, timeout: int = 60, command_timeout: float | None = None
    ) -> ExecutionResult:
        """Run a shell command and return its output and exit code.

        Args:
            command: The shell command to run in the sandbox.
            timeout: Seconds to wait for the sandbox to respond.
            command_timeout: Optional limit, in seconds, on how long the command
                itself may run. The sandbox kills the command when it is
                exceeded, and the read timeout is extended past it so the
                result still arrives. The legacy runtime reports this as an
                ExecutionResult with ``timed_out`` set (runtimes that predate
                the field ignore the limit). sandboxd uses it as the gRPC
                deadline and reports an exceeded deadline the same way, with
                exit code 124 and ``timed_out`` set; output written before the
                deadline is not returned. Through the sandbox router a request
                is also bounded by the router's ``--proxy-timeout`` (180s by
                default), so a longer ``command_timeout`` ends in a 502/504
                from the router instead of a ``timed_out`` result.
        """
        span = trace.get_current_span()
        if span.is_recording():
            executable = _extract_executable(command)
            span.set_attribute("sandbox.command.executable", executable)

        if self.connector.is_sandboxd():
            if command_timeout is None:
                result = self._run_sandboxd(command, timeout)
            else:
                result = self._run_sandboxd(
                    command, command_timeout, report_timeout=True)
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

    def _run_sandboxd(
        self, command: str, timeout: float, report_timeout: bool = False
    ) -> ExecutionResult:
        """Execute via sandboxd's gRPC ProcessService.

        The shell-string API is preserved by wrapping the command in
        "/bin/sh -c", matching what the legacy python-runtime did
        server-side. Generated stubs are imported lazily so the REST
        filesystem path works without grpcio installed.
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
        request = process_pb2.ExecuteRequest(
            config=process_pb2.ProcessConfig(command=["/bin/sh", "-c", command]),
        )
        try:
            response = stub.Execute(request, timeout=timeout)
        except grpc.RpcError as e:
            if report_timeout and e.code() == grpc.StatusCode.DEADLINE_EXCEEDED:
                return ExecutionResult(
                    stderr=f"Command timed out after {timeout:g} seconds",
                    exit_code=124,
                    timed_out=True,
                )
            if e.code() == grpc.StatusCode.UNAVAILABLE:
                self.connector.invalidate_sandboxd_transport(channel)
            raise RuntimeError(
                f"sandboxd process service failed ({e.code()}): {e.details()}"
            ) from e
        return ExecutionResult(
            stdout=response.stdout.decode("utf-8", errors="replace"),
            stderr=response.stderr.decode("utf-8", errors="replace"),
            exit_code=response.exit_code,
        )
