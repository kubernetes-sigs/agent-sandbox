import pytest
from k8s_agent_sandbox_gymnasium.termination_fns import SparseTaskTermination

def test_sparse_task_termination():
    def success_fn(obs, info):
        return "done" in obs
    
    term = SparseTaskTermination(success_fn=success_fn)
    
    assert term("not yet", {}, "task") is False
    assert term("we are done here", {}, "task") is True
