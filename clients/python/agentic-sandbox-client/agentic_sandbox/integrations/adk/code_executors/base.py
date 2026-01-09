from google.adk.agents.invocation_context import InvocationContext
from google.adk.code_executors.base_code_executor import BaseCodeExecutor
from google.adk.code_executors.code_execution_utils import CodeExecutionInput
from google.adk.code_executors.code_execution_utils import CodeExecutionResult
from google.adk.code_executors.base_code_executor import BaseCodeExecutor

from agentic_sandbox.integrations.sandbox_utils import SandboxSettings


class SandboxCodeExecutor(BaseCodeExecutor):
    """
    Base Agent Sandbox Code Executor.

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    def __init__(self, sandbox_settings: SandboxSettings):
        super().__init__()
        self._sandbox_settings = sandbox_settings
