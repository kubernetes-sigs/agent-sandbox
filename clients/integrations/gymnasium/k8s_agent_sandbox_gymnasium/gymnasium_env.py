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

import time
import logging
import string

try:
    import gymnasium as gym
    from gymnasium import spaces
except ModuleNotFoundError as e:
    raise ImportError(
        "The 'gymnasium' library is required to use 'k8s_agent_sandbox_gymnasium'. "
        "Install it via `pip install k8s-agent-sandbox-gymnasium` "
        "or install gymnasium directly: `pip install gymnasium`."
    ) from e

from typing import Optional

from k8s_agent_sandbox import SandboxClient

from .reward_fn import RewardFn
from .termination_fn import TerminationFn

logger = logging.getLogger(__name__)

class SandboxEnv(gym.Env):
    """
    A Gymnasium-compatible environment backed by a Kubernetes Agent Sandbox.

    Each episode provisions (or claims from a warmpool) a fresh isolated pod.
    The agent submits shell commands as actions; observations are the resulting
    stdout/stderr text.

    Args:
        reward_fn:      A RewardFn instance that scores each (action, obs, info, task).
        termination_fn: A TerminationFn instance that returns True if terminated and False otherwise.
        client:         An instance of k8s_agent_sandbox.SandboxClient that manages sandboxes.
        warmpool:       Name of the SandboxWarmpool to use.
        namespace:      Kubernetes namespace.
        step_timeout_seconds: A timeout in seconds for each action.
        max_episode_steps:    Hard truncation limit per episode.
        max_obs_length:       Max characters kept from stdout/stderr.
    """

    metadata = {"render_modes": []}

    def __init__(
        self,
        reward_fn: RewardFn,
        termination_fn: TerminationFn,
        client: SandboxClient,
        warmpool: str = "simple-sandbox-warmpool",
        namespace: str = "default",
        step_timeout_seconds: int = 60,
        max_episode_steps: int = 20,
        max_obs_length: int = 4096,
    ):
        super().__init__()

        if not isinstance(reward_fn, RewardFn):
            raise TypeError(f"reward_fn must be a RewardFn instance, got {type(reward_fn)}")

        if not isinstance(termination_fn, TerminationFn):
            raise TypeError(f"termination_fn must be a TerminationFn instance, got {type(termination_fn)}")

        self.reward_fn         = reward_fn
        self.termination_fn    = termination_fn
        self.warmpool          = warmpool
        self.namespace         = namespace
        self.max_episode_steps = max_episode_steps
        self.max_obs_length    = max_obs_length

        self._client               = client
        self._step_timeout_seconds = step_timeout_seconds
        self._sandbox              = None
        self._current_task         = ""
        self._step_count           = 0

        # Gymnasium spaces — text-native; wrap with TokenizedWrapper for RLlib/SB3
        self.observation_space = spaces.Text(
            max_length=max_obs_length,
            charset=string.printable
        )
        self.action_space = spaces.Text(
            max_length=2048,
            charset=string.printable
        )

    # ── Gymnasium API ──────────────────────────────────────────────────────────

    def reset(
        self,
        seed: Optional[int] = None,
        options: Optional[dict] = None,
    ):
        super().reset(seed=seed)
        options = options or {}

        # Tear down the previous sandbox if one exists
        self._close_sandbox()

        # Claim a fresh sandbox (from warmpool if template has one configured)
        self._sandbox = self._client.create_sandbox(
            warmpool=self.warmpool,
            namespace=self.namespace,
        )

        self._current_task = options.get("task", "")
        self._step_count   = 0

        # Let the reward function reset its internal state for the new episode
        self.reward_fn.reset(self._current_task)
        self.termination_fn.reset(self._current_task)

        obs  = "Sandbox ready."
        info = {
            "claim_name": getattr(self._sandbox, "claim_name", "unknown"),
            "sandbox_id": getattr(self._sandbox, "sandbox_id", "unknown"),
            "task": self._current_task,
        }
        return obs, info

    def step(self, action: str):
        if self._sandbox is None:
            raise RuntimeError("Call reset() before step().")

        self._step_count += 1
        t0 = time.monotonic()

        try:
            result     = self._sandbox.commands.run(action, timeout=self._step_timeout_seconds)
            stdout     = result.stdout or ""
            stderr     = result.stderr or ""
            exit_code  = result.exit_code
            env_error  = False
        except Exception as exc:
            stdout     = ""
            stderr     = str(exc)
            exit_code  = -1
            env_error  = True
            logger.warning("Failed to run action in sandbox (%s). Exception: %s", getattr(self._sandbox, "claim_name", "unknown"), exc)

        elapsed_ms = int((time.monotonic() - t0) * 1000)

        # Observation: prefer stdout, fall back to stderr
        parts = []
        if stdout:
            parts.append(stdout)
        if stderr:
            parts.append(f"[stderr]\n{stderr}")
        obs = "\n".join(parts)[: self.max_obs_length]

        info = {
            "exit_code":  exit_code,
            "stdout":     stdout,
            "stderr":     stderr,
            "elapsed_ms": elapsed_ms,
            "step":       self._step_count,
            "env_error":  env_error
        }

        reward     = self.reward_fn(action, obs, info, self._current_task)
        terminated = self.termination_fn(obs, info, self._current_task)
        truncated  = self._step_count >= self.max_episode_steps

        return obs, reward, terminated, truncated, info

    def close(self):
        self._close_sandbox()

    # ── Internals ─────────────────────────────────────────────────────────────

    def _close_sandbox(self):
        if self._sandbox is not None:
            try:
                self._sandbox.terminate()
            except Exception as e:
                logger.warning("Failed to terminate sandbox: %s", e)
            self._sandbox = None
