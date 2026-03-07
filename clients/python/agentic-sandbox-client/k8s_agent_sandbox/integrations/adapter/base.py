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

from abc import (
    ABC,
    abstractmethod,
)
from enum import Enum
import logging
import traceback

from pydantic import BaseModel, Field

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations.sandbox_settings import BaseSandboxSettings
from k8s_agent_sandbox.integrations.sandbox_settings import SandboxSettings

logger = logging.getLogger(__name__)

SANDBOX_ERROR_MESSAGE = "Sandbox execution completed with an error."

class BaseSandboxIntegrationAdapter(ABC):
    """
    Base Sandbox Executor class. In can be subclassed in order implement some interaction with a sandbox.

    Args:
        sandbox_settings: Settings to create a sandbox.

    Attributes:
        NAME: Class attribute with adapter's name.
        DESCRIPTION: Class attribute with tool adataper's description.
        INPUT_SCHEMA: Class attribute with adapter's input schema.
        RESULT_SCHEMA: Class Attribute with adapter's result schema.
    """

    NAME: str
    DESCRIPTION: str
    INPUT_SCHEMA: type[BaseModel]
    RESULT_SCHEMA: type[BaseModel]

    def __init__(self, sandbox_settings: BaseSandboxSettings):
        self._sandbox_settings = sandbox_settings

    def execute_as_tool(self, **args: str) -> dict:
        """
        Execute sandbox as a common tool with JSON serializable results and errors.

        Args:
            timeout: Execution timeout.
            args: Tool parameters that has to be passed by the LLM when it calls this tool.

        Returns:
            JSON serializable dict that can be handled by a common agent tool.
        """
        try:
            result = self.execute(
                **args,
            )
        except Exception as e:
            logger.exception(SANDBOX_ERROR_MESSAGE)
            return sandbox_error_to_json(e)

        return sandbox_result_to_json(result)

    @abstractmethod
    def execute(self, **args) -> ExecutionResult:
        """
        The place for an actual implementation of sandbox execution
        """


class SandboxIntegrationAdapter(BaseSandboxIntegrationAdapter):
    """Base Sandbox Integration Executor that uses normal sandbox client."""

    def __init__(self, sandbox_settings: SandboxSettings):
        super().__init__(sandbox_settings)


def sandbox_result_to_json(execution_result: ExecutionResult):
    """
    Convert sandbox result to a JSON serializable format
    that can be used by the most of the agent tools,
    """
    return {
        "status": (
            ExecutionResultStatus.SUCCESS.value
            if execution_result.exit_code == 0
            else ExecutionResultStatus.FAILURE.value
        ),
        "exit_code": execution_result.exit_code,
        "stdout": execution_result.stdout,
        "stderr": execution_result.stderr,
    }


def sandbox_error_to_json(error: Exception):
    """
    Return JSON serializable error message that
    can be used by the most of the agent tools.
    """

    return {
        "status": ExecutionResultStatus.ERROR.value,
        "sandbox_error": create_sandbox_error_message_with_traceback(error),
    }


def create_sandbox_error_message_with_traceback(error: Exception) -> str:
    """Compose a script with an error message and erro traceback"""

    traceback_string = "".join(traceback.format_exception(error))
    return f"{SANDBOX_ERROR_MESSAGE}\nTraceback:\n{traceback_string}"


class CommonBaseInputSchema(BaseModel):
    timeout: int = Field(
        default=60,
        description="Timeout in seconds which stops execution with error when reached.",
    )


class ExecutionResultStatus(str, Enum):
    SUCCESS = "success"
    FAILURE = "failure"
    ERROR = "error"


EXECUTION_RESULT_STATUS_DESCRIPTION = f"""
The execution status with the next possible values:
  * {ExecutionResultStatus.SUCCESS.value}: Execution is successful.
  * {ExecutionResultStatus.FAILURE.value}: Execution completed but with failure (non-zero return code).
  * {ExecutionResultStatus.ERROR.value}: Sandbox failed with unexpected internal error.
"""


class CommonExecutionResultSchema(BaseModel):
    status: ExecutionResultStatus = Field(
        description=EXECUTION_RESULT_STATUS_DESCRIPTION
    )
    exit_code: int | None = Field(
        description=f"Exit code of the executed process. Presented only when 'status' is not '{ExecutionResultStatus.ERROR.value}'."
    )
    stdout: str | None = Field(
        description=f"Stdout of the executed code. Presented only when 'status' is not '{ExecutionResultStatus.ERROR.value}'."
    )
    stderr: str | None = Field(
        description=f"Stderr of the executed code. Presented only when 'status' is not '{ExecutionResultStatus.ERROR.value}'."
    )
    sandbox_error: str | None = Field(
        description=f"Text of the sandbox internal error. Only present when 'status' is '{ExecutionResultStatus.ERROR.value}'."
    )
