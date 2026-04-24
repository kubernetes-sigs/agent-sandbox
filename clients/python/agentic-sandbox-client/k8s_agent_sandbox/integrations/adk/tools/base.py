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
from google.genai import types
from google.adk.tools import BaseTool, ToolContext

from k8s_agent_sandbox.integrations.sandbox_settings import (
    SandboxSettings,
    BaseSandboxSettings,
)
from k8s_agent_sandbox.integrations.adapter.base import BaseSandboxIntegrationAdapter


class BaseADKSandboxTool(BaseTool):
    """
    A subclass of ADK's 'BaseTool' that can interact with Agent Sandbox.

    Attributes:
        SANDBOX_ADAPTER_CLS: Class of the adapter that has to hadnle actual execution of a sandbox.

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    SANDBOX_ADAPTER_CLS: type[BaseSandboxIntegrationAdapter]

    def __init__(
        self,
        sandbox_settings: BaseSandboxSettings,
    ):
        super().__init__(
            name=self.__class__.SANDBOX_ADAPTER_CLS.NAME,
            description=self.__class__.SANDBOX_ADAPTER_CLS.DESCRIPTION,
        )

        self._sandbox_settings = sandbox_settings
        adapter_cls = self.__class__.SANDBOX_ADAPTER_CLS
        self._adapter = adapter_cls(self._sandbox_settings)

    async def run_async(
        self, *, args: dict[str, Any], tool_context: ToolContext
    ) -> Any:
        return self._adapter.execute_as_tool(**args)

    def _get_declaration(self):
        return types.FunctionDeclaration(
            name=self.name,
            description=self.description,
            parameters_json_schema=self._adapter.__class__.INPUT_SCHEMA.model_json_schema(),
            response_json_schema=self._adapter.__class__.RESULT_SCHEMA.model_json_schema(),
        )


class ADKSandboxTool(BaseADKSandboxTool):
    """
    Base ADK sandbox class that uses normal sandbox client.
    """

    def __init__(self, sandbox_settings: SandboxSettings):
        super().__init__(sandbox_settings)
