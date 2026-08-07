import pytest
from unittest.mock import MagicMock
import gymnasium as gym

from k8s_agent_sandbox_gymnasium.gymnasium_env import SandboxEnv
from k8s_agent_sandbox_gymnasium.reward_fn import RewardFn
from k8s_agent_sandbox_gymnasium.termination_fn import TerminationFn

class MockReward(RewardFn):
    def __init__(self):
        self.reset_calls = 0
    def reset(self, task):
        self.reset_calls += 1
    def __call__(self, action, obs, info, task):
        return 1.0

class MockTermination(TerminationFn):
    def __init__(self):
        self.reset_calls = 0
    def reset(self, task):
        self.reset_calls += 1
    def __call__(self, obs, info, task):
        return False

@pytest.fixture
def mock_sandbox():
    sandbox = MagicMock()
    sandbox.claim_name = "test-claim"
    sandbox.sandbox_id = "test-id"
    
    # Mock command result
    result = MagicMock()
    result.stdout = "test output"
    result.stderr = ""
    result.exit_code = 0
    
    sandbox.commands.run.return_value = result
    return sandbox

@pytest.fixture
def mock_client(mock_sandbox):
    client = MagicMock()
    client.create_sandbox.return_value = mock_sandbox
    return client

def test_env_initialization(mock_client):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client,
        max_episode_steps=10
    )
    
    assert env.reward_fn is reward_fn
    assert env.termination_fn is termination_fn
    assert env.max_episode_steps == 10
    assert isinstance(env.observation_space, gym.spaces.Text)
    assert isinstance(env.action_space, gym.spaces.Text)

def test_env_invalid_reward():
    termination_fn = MockTermination()
    
    with pytest.raises(TypeError, match="must be a RewardFn instance"):
        SandboxEnv(
            reward_fn="not a reward fn",
            termination_fn=termination_fn
        )

def test_env_reset(mock_client, mock_sandbox):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client
    )
    
    obs, info = env.reset(options={"task": "do something"})
    
    # Check that old sandbox is closed if it existed (in this case, it was None)
    mock_client.create_sandbox.assert_called_once_with(
        warmpool="simple-sandbox-warmpool",
        namespace="default"
    )
    
    assert obs == "Sandbox ready."
    assert info["claim_name"] == "test-claim"
    assert info["sandbox_id"] == "test-id"
    assert info["task"] == "do something"
    
    assert reward_fn.reset_calls == 1
    assert termination_fn.reset_calls == 1

def test_env_step(mock_client, mock_sandbox):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client,
        max_episode_steps=5
    )
    
    env.reset(options={"task": "test-task"})
    
    obs, reward, terminated, truncated, info = env.step("echo hello")
    
    mock_sandbox.commands.run.assert_called_once_with("echo hello")
    
    assert obs == "test output"
    assert reward == 1.0
    assert not terminated
    assert not truncated
    assert info["exit_code"] == 0
    assert info["stdout"] == "test output"
    assert info["stderr"] == ""
    assert info["step"] == 1
    assert not info["env_error"]

def test_env_step_error_handling(mock_client, mock_sandbox):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client
    )
    
    env.reset()
    
    # Simulate an exception during run
    mock_sandbox.commands.run.side_effect = Exception("Connection lost")
    
    obs, reward, terminated, truncated, info = env.step("echo hello")
    
    assert obs == "Connection lost"
    assert info["exit_code"] == -1
    assert info["env_error"] is True
    assert info["stderr"] == "Connection lost"

def test_env_step_before_reset(mock_client):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client
    )
    
    with pytest.raises(RuntimeError, match="Call reset\\(\\) before step\\(\\)."):
        env.step("echo hello")

def test_env_truncation(mock_client, mock_sandbox):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client,
        max_episode_steps=2
    )
    
    env.reset()
    
    # Step 1
    obs, reward, terminated, truncated, info = env.step("cmd1")
    assert not truncated
    
    # Step 2 (should truncate)
    obs, reward, terminated, truncated, info = env.step("cmd2")
    assert truncated

def test_env_close(mock_client, mock_sandbox):
    reward_fn = MockReward()
    termination_fn = MockTermination()
    
    env = SandboxEnv(
        reward_fn=reward_fn,
        termination_fn=termination_fn,
        client=mock_client
    )
    
    env.reset()
    env.close()
    
    mock_sandbox.terminate.assert_called_once()
    assert env._sandbox is None
