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

from k8s_agent_sandbox_gymnasium.reward_fns import (
    ExitCodeReward,
    SparseTaskReward,
    StepPenaltyReward,
    FileCreatedReward
)
from k8s_agent_sandbox_gymnasium.reward_fn import RewardFn

def test_exit_code_reward():
    reward = ExitCodeReward()
    assert reward("cmd", "obs", {"exit_code": 0}, "task") == 1.0
    assert reward("cmd", "obs", {"exit_code": 1}, "task") == -1.0

def test_sparse_task_reward():
    def success_fn(obs, info):
        return "success" in obs and info.get("exit_code") == 0

    reward = SparseTaskReward(success_fn=success_fn)
    assert reward("cmd", "some success output", {"exit_code": 0}, "task") == 1.0
    assert reward("cmd", "some failure output", {"exit_code": 0}, "task") == 0.0
    assert reward("cmd", "some success output", {"exit_code": 1}, "task") == 0.0

def test_step_penalty_reward():
    base_reward = ExitCodeReward()
    reward = StepPenaltyReward(base=base_reward, penalty=0.1)

    # success: 1.0 - 0.1 = 0.9
    assert reward("cmd", "obs", {"exit_code": 0}, "task") == 0.9
    # failure: -1.0 - 0.1 = -1.1
    assert reward("cmd", "obs", {"exit_code": 1}, "task") == -1.1

    # check reset propagation
    class MockBase(RewardFn):
        def __init__(self):
            self.reset_called = False
            self.task = None
        def reset(self, task):
            self.reset_called = True
            self.task = task
        def __call__(self, action, obs, info, task):
            return 0.0

    mock_base = MockBase()
    reward_mock = StepPenaltyReward(base=mock_base, penalty=0.5)
    reward_mock.reset("test_task")
    assert mock_base.reset_called is True
    assert mock_base.task == "test_task"

def test_file_created_reward():
    reward = FileCreatedReward(target_file="test.txt")

    # First time file is seen
    assert reward("ls", "test.txt is here", {}, "task") == 1.0

    # Second time file is seen (already awarded)
    assert reward("ls", "test.txt is still here", {}, "task") == 0.0

    # Resetting the reward state
    reward.reset("new_task")

    # File not seen
    assert reward("ls", "no file here", {}, "new_task") == 0.0

    # First time file is seen in new task
    assert reward("ls", "test.txt", {}, "new_task") == 1.0
