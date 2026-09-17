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

"""Adapt the text-native SandboxEnv to finite spaces suitable for RLlib."""

from dataclasses import dataclass
import operator
import re
import shlex

import gymnasium as gym
from gymnasium import spaces
import numpy as np

from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import (
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)
from k8s_agent_sandbox_gymnasium import RewardFn, SandboxEnv, TerminationFn


CREATE_DIRECTORY = 0
WRITE_FILE = 1
REMOVE_OUTPUT = 2

ACTION_NAMES = {
    CREATE_DIRECTORY: "create-directory",
    WRITE_FILE: "write-file",
    REMOVE_OUTPUT: "remove-output",
}

TASK_ROOT = "/tmp/agent-sandbox-rllib"
OUTPUT_DIRECTORY = f"{TASK_ROOT}/output"
OUTPUT_FILE = f"{OUTPUT_DIRECTORY}/answer.txt"
EXPECTED_CONTENT = "sandbox-ready"
STATE_MARKER = "FILE_TASK_STATE"
TASK_DESCRIPTION = (
    f"Create {OUTPUT_FILE} with the exact content {EXPECTED_CONTENT!r}."
)

_STATE_PATTERN = re.compile(
    rf"(?:^|\n){STATE_MARKER}=([01]),([01]),([01])(?:\n|$)"
)

# Emit a stable marker so arbitrary command output never becomes model input.
_INSPECT_STATE_COMMAND = f"""
directory_exists=0
file_exists=0
content_is_correct=0
if [ -d {OUTPUT_DIRECTORY} ]; then directory_exists=1; fi
if [ -f {OUTPUT_FILE} ]; then file_exists=1; fi
if [ "$file_exists" -eq 1 ] && [ "$(cat {OUTPUT_FILE})" = "{EXPECTED_CONTENT}" ]; then
  content_is_correct=1
fi
printf '{STATE_MARKER}=%s,%s,%s\\n' \
  "$directory_exists" "$file_exists" "$content_is_correct"
""".strip()

_ACTION_COMMANDS = {
    CREATE_DIRECTORY: f"mkdir -p {OUTPUT_DIRECTORY}",
    WRITE_FILE: (
        f"if [ -d {OUTPUT_DIRECTORY} ]; then "
        f"printf '%s\\n' '{EXPECTED_CONTENT}' > {OUTPUT_FILE}; fi"
    ),
    REMOVE_OUTPUT: f"rm -rf {TASK_ROOT}",
}


@dataclass(frozen=True)
class FileTaskState:
    """The task state encoded by a Sandbox command observation."""

    directory_exists: bool
    file_exists: bool
    content_is_correct: bool

    def observation(self, remaining_steps: float) -> np.ndarray:
        """Return the numeric observation consumed by the RLlib policy."""
        return np.asarray(
            [
                self.directory_exists,
                self.file_exists,
                self.content_is_correct,
                remaining_steps,
            ],
            dtype=np.float32,
        )


EMPTY_STATE = FileTaskState(False, False, False)


def parse_file_task_state(observation: str) -> FileTaskState:
    """Extract the stable task-state marker from Sandbox stdout."""
    match = _STATE_PATTERN.search(observation)
    if match is None:
        raise ValueError(f"Sandbox observation is missing {STATE_MARKER}")
    return FileTaskState(*(value == "1" for value in match.groups()))


def _action_id(action: int) -> int:
    """Return a valid integer action without coercing other value types."""
    try:
        action_id = operator.index(action)
    except TypeError as exc:
        raise ValueError(f"Unsupported file-task action: {action!r}") from exc
    if action_id not in _ACTION_COMMANDS:
        raise ValueError(f"Unsupported file-task action: {action!r}")
    return action_id


def command_for_action(action: int) -> str:
    """Translate a finite policy action into a fixed shell command."""
    task_command = _ACTION_COMMANDS[_action_id(action)]
    # The legacy Python runtime executes argv directly, so explicitly invoke a
    # shell for conditionals and redirection. shlex.quote keeps the script one
    # argument after the runtime parses the command string.
    script = f"{task_command}\n{_INSPECT_STATE_COMMAND}"
    return f"sh -c {shlex.quote(script)}"


class FileTaskReward(RewardFn):
    """Reward successful file creation and penalize wasted or failed steps."""

    def __call__(self, action, obs, info, task):
        del action, task
        if info.get("env_error"):
            return -1.0
        try:
            state = parse_file_task_state(obs)
        except ValueError:
            return -1.0
        return 1.0 if state.content_is_correct else -0.05


class FileTaskTermination(TerminationFn):
    """Finish an episode once the expected file content exists."""

    def __call__(self, obs, info, task):
        del task
        if info.get("env_error"):
            return False
        try:
            return parse_file_task_state(obs).content_is_correct
        except ValueError:
            return False


class DiscreteFileTaskWrapper(gym.Wrapper):
    """Map fixed integer actions and text output to RLlib-friendly spaces."""

    def __init__(self, env, *, client=None, max_episode_steps=4):
        super().__init__(env)
        if max_episode_steps < 1:
            raise ValueError("max_episode_steps must be at least one")
        self.action_space = spaces.Discrete(len(ACTION_NAMES))
        self.observation_space = spaces.Box(
            low=0.0,
            high=1.0,
            shape=(4,),
            dtype=np.float32,
        )
        self._client = client
        self._max_episode_steps = int(max_episode_steps)
        self._last_state = EMPTY_STATE

    def reset(self, *, seed=None, options=None):
        _, info = self.env.reset(
            seed=seed,
            options={"task": TASK_DESCRIPTION, **(options or {})},
        )
        # SandboxEnv provisions a fresh claim, so the task starts empty.
        self._last_state = EMPTY_STATE
        return self._last_state.observation(1.0), info

    def step(self, action):
        action_id = _action_id(action)

        sandbox_observation, reward, terminated, truncated, info = self.env.step(
            command_for_action(action_id)
        )
        info = dict(info)

        try:
            state = parse_file_task_state(sandbox_observation)
            info["state_parse_error"] = False
        except ValueError as exc:
            # Keep the observation valid until RLlib truncates and resets.
            state = self._last_state
            info["state_parse_error"] = True
            info["state_parse_error_message"] = str(exc)

        self._last_state = state
        step_count = int(info.get("step", 0))
        remaining_steps = max(
            0.0,
            (self._max_episode_steps - step_count) / self._max_episode_steps,
        )

        info.update(
            {
                "action_name": ACTION_NAMES[action_id],
                "sandbox_observation": sandbox_observation,
                "success": state.content_is_correct,
            }
        )

        # A connection or state-decoding failure cannot produce a useful next
        # action. Truncating forces RLlib to reset the episode and replace the
        # SandboxClaim.
        truncated = bool(
            truncated
            or info.get("env_error")
            or info["state_parse_error"]
        )
        return (
            state.observation(remaining_steps),
            reward,
            bool(terminated),
            truncated,
            info,
        )

    def close(self):
        try:
            super().close()
        finally:
            if self._client is not None:
                self._client.delete_all()


def _connection_config(mode: str, router_namespace: str):
    if mode == "tunnel":
        return SandboxLocalTunnelConnectionConfig(
            router_namespace=router_namespace,
        )
    if mode == "in-cluster":
        return SandboxInClusterConnectionConfig()
    raise ValueError(
        f"Unsupported connection mode {mode!r}; expected 'tunnel' or 'in-cluster'"
    )


class SandboxFileTaskEnv(DiscreteFileTaskWrapper):
    """Construct an independent SandboxEnv inside each RLlib EnvRunner."""

    def __init__(self, config=None):
        config = dict(config or {})
        max_episode_steps = int(config.get("max_episode_steps", 4))
        if max_episode_steps < 1:
            raise ValueError("max_episode_steps must be at least one")
        # RLlib constructs this class inside each EnvRunner process, giving
        # every runner an independent Kubernetes client and claim lifecycle.
        client = SandboxClient(
            connection_config=_connection_config(
                config.get("connection_mode", "tunnel"),
                config.get("router_namespace", "agent-sandbox-system"),
            ),
            cleanup=True,
        )

        try:
            sandbox_env = SandboxEnv(
                reward_fn=FileTaskReward(),
                termination_fn=FileTaskTermination(),
                client=client,
                warmpool=config.get("warmpool", "simple-sandbox-warmpool"),
                namespace=config.get("namespace", "gymnasium"),
                step_timeout_seconds=int(
                    config.get("step_timeout_seconds", 60)
                ),
                max_episode_steps=max_episode_steps,
            )
        except Exception:
            client.delete_all()
            raise

        super().__init__(
            sandbox_env,
            client=client,
            max_episode_steps=max_episode_steps,
        )
