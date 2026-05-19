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


from pydantic import Field

from k8s_agent_sandbox.integrations import ComputerUseSandboxSettings
from k8s_agent_sandbox.sandbox_client import ExecutionResult
from .base import (
    BaseSandboxIntegrationAdapter,
    CommonBaseInputSchema,
    CommonExecutionResultSchema,
)


class _InputSchema(CommonBaseInputSchema):
    query: str = Field(
        description="String with a natural language query to execute within the sandbox."
    )


class ComputerUseSandboxIntegrationAdapter(BaseSandboxIntegrationAdapter):
    """
    Sandbox Executor that executes computer use queries.
    """

    NAME = "execute_action_in_sandbox"
    DESCRIPTION = (
        "Executes natural language query in a sandbox and returns execution results."
    )

    INPUT_SCHEMA = _InputSchema

    RESULT_SCHEMA = CommonExecutionResultSchema

    def __init__(self, sandbox_settings: ComputerUseSandboxSettings):
        super().__init__(sandbox_settings)

    def _execute_query(self, query: str, timeout: int = 60) -> ExecutionResult:
        with self._sandbox_settings.create_client() as sandbox:
            result = sandbox.agent(query, timeout)
            return result

    def execute(self, **args) -> ExecutionResult:
        return self._execute_query(**args)
