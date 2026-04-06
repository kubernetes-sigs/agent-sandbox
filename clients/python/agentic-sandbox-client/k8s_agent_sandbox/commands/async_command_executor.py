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

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.models import ExecutionResult
from k8s_agent_sandbox.trace_manager import async_trace_span, trace


class AsyncCommandExecutor:
    """
    Handles async execution of commands within the sandbox.
    """

    def __init__(self, connector: AsyncSandboxConnector, tracer, trace_service_name: str):
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @async_trace_span("run")
    async def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.command", command)

        payload = {"command": command}
        response = await self.connector.send_request(
            "POST", "execute", json=payload, timeout=timeout
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
