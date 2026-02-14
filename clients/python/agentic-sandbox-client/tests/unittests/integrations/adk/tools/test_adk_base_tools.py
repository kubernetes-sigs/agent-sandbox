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

import asyncio
from unittest import mock

import pytest

from agentic_sandbox.integrations.adk.tools.base import (
    SandboxFunctionTool,
    PredefinedSandboxFunctionTool,
)

from test_utils.integrations.sandbox_tests_base import SandboxTestBase


@pytest.mark.parametrize("description", [("some description"), (None)])
@pytest.mark.parametrize("tool_type", [("function"), ("predefined_function")])
class TestBaseFunctions(SandboxTestBase):
    def test_tool(self, description, tool_type):

        def sample_tool_func(**kwargs):
            """Tool description"""

            sandbox_settings = kwargs.get("sandbox")
            if sandbox_settings is None:
                return {
                    "status": "error",
                    "message": "Sandbox settings are not in the kwargs",
                }
            else:
                return {"status": "ok", "message": "All is ok"}

        tool = self._create_tool(
            tool_type,
            sample_tool_func,
            tool_description=description,
        )

        if description:
            expected_description = description
        else:
            expected_description = sample_tool_func.__doc__

        assert tool.func.__doc__ == expected_description

        result = asyncio.run(tool.run_async(args={}, tool_context=mock.MagicMock()))

        assert result["status"] == "ok", result["message"]

    def _create_tool(
        self, tool_type, tool_func, tool_description=None
    ) -> SandboxFunctionTool:
        if tool_type == "function":
            return SandboxFunctionTool(
                self.sandbox_settings_mock, tool_func, description=tool_description
            )
        elif tool_type == "predefined_function":

            class MyTool(PredefinedSandboxFunctionTool):
                func = tool_func
                description = tool_description

            return MyTool(self.sandbox_settings_mock)
        else:
            raise ValueError(f"Wrong tool type: {tool_type}")
