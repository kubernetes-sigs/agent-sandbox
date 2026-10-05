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

"""Non-blocking command execution for legacy and sandboxd runtimes."""

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.commands.command_executor import _execute_request
from k8s_agent_sandbox.models import ExecutionResult
from k8s_agent_sandbox.trace_manager import async_trace_span, trace


def _extract_executable(command: str) -> str:
    """Extract a low-cardinality executable name for tracing."""
    if not command:
        return ""
    for field in command.split():
        # Skip leading inline environment variables (e.g., KEY=VALUE)
        if "=" in field:
            continue
        # Extract base executable name (strip directory paths)
        return field.split("/")[-1]
    return ""


class AsyncCommandExecutor:
    """Run commands through legacy HTTP or sandboxd's gRPC service."""

    def __init__(
        self, connector: AsyncSandboxConnector, tracer: Any, trace_service_name: str
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @async_trace_span("run")
    async def run(
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
                the field ignore the limit); sandboxd uses it as the gRPC
                deadline and raises RuntimeError when it is exceeded.
        """
        span = trace.get_current_span()
        if span.is_recording():
            executable = _extract_executable(command)
            span.set_attribute("sandbox.command.executable", executable)

        if self.connector.is_sandboxd():
            result = await self._run_sandboxd(
                command, timeout if command_timeout is None else command_timeout
            )
            if span.is_recording():
                span.set_attribute("sandbox.exit_code", result.exit_code)
            return result

        payload, read_timeout = _execute_request(command, timeout, command_timeout)
        response = await self.connector.send_request(
            "POST", "execute", json=payload, timeout=read_timeout
        )

        try:
            response_data = response.json()
        except ValueError as e:
            raise RuntimeError(
                f"Failed to decode JSON response from sandbox: {response.text}"
            ) from e
        try:
            result = ExecutionResult(**response_data)
        except Exception as e:
            raise RuntimeError(
                f"Server returned invalid execution result format: {response_data}"
            ) from e

        if span.is_recording():
            span.set_attribute("sandbox.exit_code", result.exit_code)
        return result

    async def _run_sandboxd(self, command: str, timeout: float) -> ExecutionResult:
        """Execute through sandboxd while preserving the shell-string API."""
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

        await self.connector.connect()
        channel = await self.connector.grpc_channel()
        stub = process_pb2_grpc.ProcessServiceStub(channel)
        request = process_pb2.ExecuteRequest(
            config=process_pb2.ProcessConfig(command=["/bin/sh", "-c", command])
        )
        try:
            response = await stub.Execute(request, timeout=timeout)
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.UNAVAILABLE:
                await self.connector.invalidate_sandboxd_transport(channel)
            raise RuntimeError(
                f"sandboxd process service failed ({e.code()}): {e.details()}"
            ) from e
        return ExecutionResult(
            stdout=response.stdout.decode("utf-8", errors="replace"),
            stderr=response.stderr.decode("utf-8", errors="replace"),
            exit_code=response.exit_code,
        )
