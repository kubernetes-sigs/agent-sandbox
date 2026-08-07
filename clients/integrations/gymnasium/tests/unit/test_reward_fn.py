import pytest
from k8s_agent_sandbox_gymnasium.reward_fn import RewardFn

class DummyReward(RewardFn):
    def __call__(self, action, obs, info, task):
        return 1.0

def test_reward_fn_reset():
    reward = DummyReward()
    # Default reset does nothing, just check it doesn't crash
    reward.reset("some_task")

def test_reward_fn_call():
    reward = DummyReward()
    assert reward("ls", "obs", {}, "task") == 1.0

def test_reward_fn_abstract():
    with pytest.raises(TypeError):
        RewardFn()
