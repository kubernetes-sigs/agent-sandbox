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

from k8s_agent_sandbox.integrations.langchain.tools import (
    PythonLangChainSandboxTool,
)
from test_utils.integrations.sandbox_tests_base import SandboxJsonResultTest


class TestLangchainPythonSandboxToolResult(SandboxJsonResultTest):

    def _execute_in_sandbox(self):
        tool = PythonLangChainSandboxTool(sandbox_settings=self.sandbox_settings_mock)
        result = tool.invoke({"code": "some code"})
        return result
