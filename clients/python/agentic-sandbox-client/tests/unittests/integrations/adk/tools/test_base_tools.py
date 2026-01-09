from typing import Optional
import asyncio
from unittest import mock

import pytest

from agentic_sandbox.sandbox_client import ExecutionResult
from agentic_sandbox.integrations.adk.tools.base import (
    SandboxFunctionTool,
    PredefinedSandboxFunctionTool,
)


class TestBaseTools:
    @pytest.mark.parametrize("creation_type", [("from_function"), ("from_class")])
    @pytest.mark.parametrize("description", [("some description"), (None)])
    def test_tool(self, description, creation_type, sandbox_settings_mock):

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
            sample_tool_func,
            creation_type,
            sandbox_settings_mock,
            tool_description=description,
        )

        if description:
            expected_description = description
        else:
            expected_description = sample_tool_func.__doc__

        assert tool.func.__doc__ == expected_description

        result = asyncio.run(
            tool.run_async(args={"code": "some code"}, tool_context=mock.MagicMock())
        )

        assert result["status"] == "ok", result["message"]

    def _create_tool(
        self,
        tool_func,
        creation_type: str,
        sandbox_settings,
        tool_description: Optional[str] = None,
    ):
        if creation_type == "from_function":
            return SandboxFunctionTool(
                sandbox_settings, tool_func, description=tool_description
            )
        elif creation_type == "from_class":

            class MyTool(PredefinedSandboxFunctionTool):
                func = tool_func
                description = tool_description

            return MyTool(sandbox_settings)
        else:
            raise ValueError("Wrong creation type")
