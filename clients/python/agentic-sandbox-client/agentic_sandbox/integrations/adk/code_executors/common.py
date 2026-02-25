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

from google.adk.code_executors.code_execution_utils import CodeExecutionResult
from google.adk.code_executors.base_code_executor import BaseCodeExecutor

from agentic_sandbox.integrations.sandbox_utils import SandboxSettings
from agentic_sandbox.sandbox_client import ExecutionResult


class SandboxCodeExecutor(BaseCodeExecutor):
    """
    Base Agent Sandbox Code Executor.

    Args:
        sandbox_settings: Settings for a sandbox to create.
    """

    def __init__(self, sandbox_settings: SandboxSettings):
        super().__init__()
        self._sandbox_settings = sandbox_settings


def sandbox_result_to_code_executor_result(result: ExecutionResult):
    """Creates code executor result from sandbox execution result"""
    return CodeExecutionResult(
        stdout=result.stdout,
        stderr=result.stderr,
    )


def sandbox_error_to_code_executor_error(error: Exception):
    """Creates code executor result from sandbox execution error"""
    return CodeExecutionResult(
        stderr=f"Sandbox error: {str(error)}",
    )
