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
