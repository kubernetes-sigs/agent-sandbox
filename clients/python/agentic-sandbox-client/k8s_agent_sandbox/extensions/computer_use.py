# Copyright 2025 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law of agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from ..sandbox_client import SandboxClient
from ..sandbox import Sandbox
from ..models import ExecutionResult
from ..trace_manager import trace_span

class SandboxWithComputerUseSupport(Sandbox):
    @trace_span("agent_query")
    def agent(self, query: str, timeout: int = 60) -> ExecutionResult:
        """Executes a query using the agent."""
        if not self.is_active:
            raise ConnectionError("Sandbox is not active. Cannot execute agent queries.")

        payload = {"query": query}

        response = self.connector.send_request("POST", "agent", json=payload, timeout=timeout)

        response_data = response.json()
        # Pydantic safely falls back to defaults for any missing keys
        return ExecutionResult(**(response_data or {}))

class ComputerUseSandboxClient(SandboxClient[SandboxWithComputerUseSupport]):
    """
    A specialized Sandbox client for the computer-use example.
    """
    sandbox_class = SandboxWithComputerUseSupport
