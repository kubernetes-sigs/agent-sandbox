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

from agentic_sandbox.integrations.sandbox_utils import sandbox_in_kwargs

from agentic_sandbox.integrations.sandbox_utils import SandboxSettings


def test_sandbox_in_kwargs_decorator():
    settings = SandboxSettings(
        template_name="some template",
        namespace="some namespace",
    )

    def func(**kwargs):
        return kwargs

    func_with_sandbox = sandbox_in_kwargs(settings)(func)
    func_kwargs = func_with_sandbox()

    assert "sandbox" in func_kwargs
    assert func_kwargs["sandbox"] is settings
