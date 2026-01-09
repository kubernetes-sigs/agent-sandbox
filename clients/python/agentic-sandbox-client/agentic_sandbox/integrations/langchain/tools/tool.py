from langchain.tools import tool

from agentic_sandbox.integrations.sandbox_utils import sandbox_in_kwargs


def sandbox_tool(sandbox_settings, description=None):
    """
    Create Langchain tool that runs Python code inside the Agent Sandbox

    Args:
        sandbox_settings: Settings to create a sandbox.
        description: Tool description.

    """

    def _create_wrapper(func):

        return tool(sandbox_in_kwargs(sandbox_settings)(func), description=description)

    return _create_wrapper
