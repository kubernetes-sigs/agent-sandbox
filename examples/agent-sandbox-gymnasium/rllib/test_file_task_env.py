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

"""Unit tests for the RLlib-to-SandboxEnv adaptation layer."""

import gymnasium as gym
from gymnasium import spaces
import numpy as np
import pytest

from file_task_env import (
    CREATE_DIRECTORY,
    DiscreteFileTaskWrapper,
    FileTaskReward,
    FileTaskTermination,
    REMOVE_OUTPUT,
    WRITE_FILE,
    command_for_action,
    parse_file_task_state,
)


class FakeSandboxEnv(gym.Env):
    def __init__(self):
        self.action_space = spaces.Text(max_length=2048)
        self.observation_space = spaces.Text(max_length=4096)
        self.commands = []
        self.closed = False
        self.step_count = 0
        self.state = [0, 0, 0]

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        self.step_count = 0
        self.state = [0, 0, 0]
        return "Sandbox ready.", {"claim_name": "sandbox-claim-test"}

    def step(self, command):
        self.commands.append(command)
        self.step_count += 1
        if "mkdir -p /tmp/agent-sandbox-rllib/output" in command:
            self.state[0] = 1
        elif "rm -rf /tmp/agent-sandbox-rllib" in command:
            self.state = [0, 0, 0]
        elif "> /tmp/agent-sandbox-rllib/output/answer.txt" in command and self.state[0]:
            self.state[1:] = [1, 1]

        observation = "FILE_TASK_STATE=" + ",".join(map(str, self.state)) + "\n"
        terminated = bool(self.state[2])
        reward = 1.0 if terminated else -0.05
        return (
            observation,
            reward,
            terminated,
            self.step_count >= 4 and not terminated,
            {"step": self.step_count, "env_error": False},
        )

    def close(self):
        self.closed = True


class FakeClient:
    def __init__(self):
        self.delete_all_calls = 0

    def delete_all(self):
        self.delete_all_calls += 1


def test_parse_file_task_state():
    state = parse_file_task_state("FILE_TASK_STATE=1,0,1\n")

    assert state.directory_exists
    assert not state.file_exists
    assert state.content_is_correct


def test_parse_file_task_state_rejects_missing_marker():
    with pytest.raises(ValueError, match="missing FILE_TASK_STATE"):
        parse_file_task_state("command failed")


def test_command_for_action_rejects_unknown_action():
    with pytest.raises(ValueError, match="Unsupported file-task action"):
        command_for_action(99)


def test_wrapper_maps_actions_and_observations():
    base_env = FakeSandboxEnv()
    client = FakeClient()
    env = DiscreteFileTaskWrapper(
        base_env,
        client=client,
        max_episode_steps=4,
    )

    observation, info = env.reset()
    assert info["claim_name"] == "sandbox-claim-test"
    np.testing.assert_array_equal(observation, [0.0, 0.0, 0.0, 1.0])

    observation, reward, terminated, truncated, info = env.step(
        CREATE_DIRECTORY
    )
    np.testing.assert_array_equal(observation, [1.0, 0.0, 0.0, 0.75])
    assert reward == -0.05
    assert not terminated
    assert not truncated
    assert info["action_name"] == "create-directory"

    observation, reward, terminated, truncated, info = env.step(WRITE_FILE)
    np.testing.assert_array_equal(observation, [1.0, 1.0, 1.0, 0.5])
    assert reward == 1.0
    assert terminated
    assert not truncated
    assert info["success"]

    env.close()
    assert base_env.closed
    assert client.delete_all_calls == 1


def test_remove_output_maps_to_fixed_command():
    command = command_for_action(REMOVE_OUTPUT)

    assert command.startswith("sh -c ")
    assert "rm -rf /tmp/agent-sandbox-rllib" in command
    assert "FILE_TASK_STATE" in command


def test_environment_error_truncates_episode_and_preserves_state():
    base_env = FakeSandboxEnv()
    env = DiscreteFileTaskWrapper(base_env, max_episode_steps=4)
    env.reset()

    def failed_step(command):
        del command
        return "connection lost", -1.0, False, False, {
            "step": 1,
            "env_error": True,
        }

    base_env.step = failed_step
    observation, reward, terminated, truncated, info = env.step(
        CREATE_DIRECTORY
    )

    np.testing.assert_array_equal(observation, [0.0, 0.0, 0.0, 0.75])
    assert reward == -1.0
    assert not terminated
    assert truncated
    assert info["state_parse_error"]


def test_close_cleans_up_client_when_environment_close_fails():
    base_env = FakeSandboxEnv()
    client = FakeClient()
    env = DiscreteFileTaskWrapper(base_env, client=client)

    def failed_close():
        raise RuntimeError("close failed")

    base_env.close = failed_close
    with pytest.raises(RuntimeError, match="close failed"):
        env.close()

    assert client.delete_all_calls == 1


def test_reward_and_termination_follow_encoded_state():
    reward = FileTaskReward()
    termination = FileTaskTermination()
    info = {"env_error": False}

    assert reward("command", "FILE_TASK_STATE=1,0,0\n", info, "task") == -0.05
    assert not termination("FILE_TASK_STATE=1,0,0\n", info, "task")
    assert reward("command", "FILE_TASK_STATE=1,1,1\n", info, "task") == 1.0
    assert termination("FILE_TASK_STATE=1,1,1\n", info, "task")
