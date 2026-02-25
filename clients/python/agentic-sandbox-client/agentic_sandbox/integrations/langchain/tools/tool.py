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

from langchain.tools import tool

from agentic_sandbox.integrations.sandbox_utils import (
    SandboxSettings,
    sandbox_in_kwargs,
)


def sandbox_tool(sandbox_settings: SandboxSettings, description=None):
    """
    Can be used as a Decorator to create a Langchain tool that can interact with the Agent Sandbox.

    Args:
        sandbox_settings: Settings to create a sandbox.
        description: Tool description.

    """

    def _create_wrapper(func):

        return tool(sandbox_in_kwargs(sandbox_settings)(func), description=description)

    return _create_wrapper
