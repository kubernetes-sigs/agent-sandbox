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

from typing import ClassVar
from typing import Generic, TypeVar
import json

from pydantic import SkipValidation
from crewai.tools import BaseTool

from k8s_agent_sandbox.integrations.sandbox_settings import (
    BaseSandboxSettings,
    SandboxSettings,
)
from k8s_agent_sandbox.integrations.adapter.base import BaseSandboxIntegrationAdapter

BaseSandboxSettingsT = TypeVar("BaseSandboxSettingsT", bound=BaseSandboxSettings)


class BaseCrewAISandboxTool(BaseTool, Generic[BaseSandboxSettingsT]):
    """
    A subclass of CrewAI 'BaseTool' that can interact with Agent Sandbox.

    Attributes:
        SANDBOX_ADAPTER_CLS: Class of the adapter that has to hadnle actual execution of a sandbox.

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    SANDBOX_ADAPTER_CLS: ClassVar[type[BaseSandboxIntegrationAdapter]]

    # Override the following base model fields to make them non-mandatory, since we set them from an adapter
    name: str | None = None  # type: ignore
    description: str | None = None  # type: ignore

    sandbox_settings: BaseSandboxSettingsT

    def __init__(self, name: str | None = None, description: str | None = None, **data):

        adapter_cls = self.__class__.SANDBOX_ADAPTER_CLS
        default_name = adapter_cls.NAME

        # Since Langchain does not provilde ability to specify the result schema,
        # we just put its json-schema to the description.
        default_description = (
            f"{adapter_cls.DESCRIPTION}\n"
            f"The JSON Schema of the result is:\n {json.dumps(adapter_cls.RESULT_SCHEMA.model_json_schema())}"
        )

        super().__init__(
            name=name or default_name,
            description=description or default_description,
            args_schema=adapter_cls.INPUT_SCHEMA,
            **data,
        )
        self._adapter = adapter_cls(self.sandbox_settings)

    def _run(self, *args, **kwargs) -> dict:
        return self._adapter.execute_as_tool(*args, **kwargs)


class CrewAISandboxTool(BaseCrewAISandboxTool):
    """
    Base CrewAI sandbox class that uses normal sandbox client.
    """

    sandbox_settings: SkipValidation[SandboxSettings]
