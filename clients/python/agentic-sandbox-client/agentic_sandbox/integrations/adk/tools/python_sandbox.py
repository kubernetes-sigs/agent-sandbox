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

from agentic_sandbox.integrations.sandbox_utils.tools import (
    COMMON_CODE_TOOL_DOCSTRING_DESCRIPTION,
)
from agentic_sandbox.integrations.sandbox_utils.python_sandbox import (
    execute_python_tool_and_handle_errors,
)
from .base import PredefinedSandboxFunctionTool


def execute_python_code_in_sandbox_tool_fn(code: str, **kwargs) -> dict:
    sandbox_settings = kwargs["sandbox"]

    return execute_python_tool_and_handle_errors(sandbox_settings, code)


class PythonSandboxTool(PredefinedSandboxFunctionTool):
    func = execute_python_code_in_sandbox_tool_fn
    description = COMMON_CODE_TOOL_DOCSTRING_DESCRIPTION
