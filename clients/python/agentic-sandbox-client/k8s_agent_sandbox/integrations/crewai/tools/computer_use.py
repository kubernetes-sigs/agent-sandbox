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

from pydantic import SkipValidation

from k8s_agent_sandbox.integrations import ComputerUseSandboxSettings
from k8s_agent_sandbox.integrations.adapter.computer_use import (
    ComputerUseSandboxIntegrationAdapter,
)
from .base import BaseCrewAISandboxTool


class ComputerUseCrewAISandboxTool(BaseCrewAISandboxTool):
    """
    A CreaAI tool that executes natural language queries in the Agent Sandbox.
    """

    SANDBOX_ADAPTER_CLS = ComputerUseSandboxIntegrationAdapter

    sandbox_settings: SkipValidation[ComputerUseSandboxSettings]
