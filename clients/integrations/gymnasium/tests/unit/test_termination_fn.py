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

import pytest
from k8s_agent_sandbox_gymnasium.termination_fn import TerminationFn

class DummyTermination(TerminationFn):
    def __call__(self, obs, info, task):
        return True

def test_termination_fn_reset():
    term = DummyTermination()
    term.reset("task")

def test_termination_fn_call():
    term = DummyTermination()
    assert term("obs", {}, "task") is True

def test_termination_fn_abstract():
    with pytest.raises(TypeError):
        TerminationFn()
