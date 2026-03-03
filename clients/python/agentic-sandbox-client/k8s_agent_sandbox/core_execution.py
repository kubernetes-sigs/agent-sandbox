# Copyright 2025 The Kubernetes Authors.
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
from .models import ExecutionResult
from .trace_manager import trace_span, trace

if TYPE_CHECKING:
    from .sandbox import Sandbox

class CoreExecution:
    """
    Handles execution of commands within the sandbox.
    """
    def __init__(self, sandbox: "Sandbox"):
        self.sandbox = sandbox

    @property
    def tracer(self):
        return self.sandbox.tracer

    @property
    def trace_service_name(self):
        return self.sandbox.trace_service_name

    @trace_span("run")
    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.command", command)

        payload = {"command": command}
        response = self.sandbox._request(
            "POST", "execute", json=payload, timeout=timeout)

        response_data = response.json()
        result = ExecutionResult(**response_data)

        if span.is_recording():
            span.set_attribute("sandbox.exit_code", result.exit_code)
        return result
