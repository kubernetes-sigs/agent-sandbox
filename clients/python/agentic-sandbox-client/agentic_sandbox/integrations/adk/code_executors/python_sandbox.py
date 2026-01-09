from google.adk.agents.invocation_context import InvocationContext
from google.adk.code_executors.base_code_executor import BaseCodeExecutor
from google.adk.code_executors.code_execution_utils import CodeExecutionInput
from google.adk.code_executors.code_execution_utils import CodeExecutionResult
from google.adk.code_executors.base_code_executor import BaseCodeExecutor

from agentic_sandbox.integrations.sandbox_utils import SandboxSettings
from .base import SandboxCodeExecutor


class PythonSandboxCodeExecutor(SandboxCodeExecutor):
    """
    An agent code executor that executes code in the Agent Sandbox

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    def execute_code(
        self,
        invocation_context: InvocationContext,
        code_execution_input: CodeExecutionInput,
    ) -> CodeExecutionResult:
        """
        Executes code in a sandbox.
        """

        with self._sandbox_settings.create_client() as sandbox:
            sandbox.write("main.py", code_execution_input.code)

            result = sandbox.run("python3 main.py")

        return CodeExecutionResult(
            stdout=result.stdout,
            stderr=result.stderr,
            output_files=[],
        )
