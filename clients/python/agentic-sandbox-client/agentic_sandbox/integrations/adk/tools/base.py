from typing import (
    Callable,
    Any,
    Optional,
)

from google.adk.tools import FunctionTool

from agentic_sandbox.integrations.sandbox_utils import (
    SandboxSettings,
    sandbox_in_kwargs,
)


class SandboxFunctionTool(FunctionTool):
    """
    A subclass of ADK's 'FunctionTool' that can interact with Agent Sandbox.

    Args:
        sandbox_settings: Settings for a sandbox to create.
        func: A function or callable to use as a tool.
        description: Optional description for a tool.
    """

    def __init__(
        self,
        sandbox_settings: SandboxSettings,
        func: Callable[..., Any],
        description: Optional[str] = None,
    ):
        func_with_sandbox = sandbox_in_kwargs(sandbox_settings)(func)
        if description:
            func_with_sandbox.__doc__ = description

        super().__init__(func_with_sandbox)


class PredefinedSandboxFunctionTool(SandboxFunctionTool):
    """
    A subclass of the 'SandboxFunctionTool' class that accepts its inputs as class attributes.
    This is used to created predefined tools from already known functions.

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    func: Callable[..., Any]
    """A function or callable to use as a tool."""

    description: Optional[str] = None
    """Optional description for a tool."""

    def __init__(
        self,
        sandbox_settings: SandboxSettings,
    ):
        super().__init__(
            sandbox_settings,
            self.__class__.func,
            description=self.__class__.description,
        )
