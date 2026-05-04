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

from k8s_agent_sandbox.integrations.adk.tools.python_sandbox import PythonADKSandboxTool

from test_utils.integrations.sandbox_tests_base import SandboxJsonResultTest


class TestADKPythonSandboxResult(SandboxJsonResultTest):

    def _execute_in_sandbox(self):
        tool = PythonADKSandboxTool(self.sandbox_settings_mock)
        result = asyncio.run(
            tool.run_async(args={"code": "some code"}, tool_context=mock.MagicMock())
        )
        return result
