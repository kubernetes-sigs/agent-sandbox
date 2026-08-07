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

from typing import TYPE_CHECKING
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import ExecutionResult
from k8s_agent_sandbox.trace_manager import trace_span, trace

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


class CommandExecutor:
    """
    Handles execution of commands within the sandbox.
    """
    def __init__(self, connector: SandboxConnector, tracer, trace_service_name: str):
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @trace_span("run")
    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        span = trace.get_current_span()
        if span.is_recording():
            executable = _extract_executable(command)
            span.set_attribute("sandbox.command.executable", executable)

        if self.connector.is_sandboxd():
            result = self._run_sandboxd(command, timeout)
            if span.is_recording():
                span.set_attribute("sandbox.exit_code", result.exit_code)
            return result

        payload = {"command": command}
        response = self.connector.send_request(
            "POST", "execute", json=payload, timeout=timeout)

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

    def _run_sandboxd(self, command: str, timeout: int) -> ExecutionResult:
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

        # Ensure the pod tunnel is established (and the gRPC target
        # published) before dialing. connect() is idempotent — it returns the
        # live tunnel or re-establishes a dead one — so this also handles
        # gRPC being the first operation and reconnecting after a teardown.
        self.connector.connect()
        channel = self.connector.grpc_channel()
        stub = process_pb2_grpc.ProcessServiceStub(channel)
        request = process_pb2.ExecuteRequest(
            config=process_pb2.ProcessConfig(command=["/bin/sh", "-c", command]),
        )
        try:
            response = stub.Execute(request, timeout=timeout)
        except grpc.RpcError as e:
            raise RuntimeError(
                f"sandboxd process service failed ({e.code()}): {e.details()}"
            ) from e
        return ExecutionResult(
            stdout=response.stdout.decode("utf-8", errors="replace"),
            stderr=response.stderr.decode("utf-8", errors="replace"),
            exit_code=response.exit_code,
        )
