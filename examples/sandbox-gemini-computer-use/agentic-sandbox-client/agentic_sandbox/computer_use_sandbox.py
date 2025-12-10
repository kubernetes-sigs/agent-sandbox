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

import requests
from agentic_sandbox.sandbox import Sandbox, ExecutionResult

class ComputerUseSandbox(Sandbox):
    """
    A specialized Sandbox client for the computer-use example.
    """
    def __init__(self, template_name: str, namespace: str = "default"):
        super().__init__(template_name, namespace)
        self.server_port = 8080

    def agent(self, query: str, timeout: int = 60) -> ExecutionResult:
        """Executes a query using the agent."""
        if not self.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot execute agent queries.")

        url = f"http://127.0.0.1:{self.server_port}/agent"
        payload = {"query": query}

        response = requests.post(url, json=payload, timeout=timeout)
        response.raise_for_status()

        response_data = response.json()
        return ExecutionResult(
            stdout=response_data['stdout'],
            stderr=response_data['stderr'],
            exit_code=response_data['exit_code']
        )
