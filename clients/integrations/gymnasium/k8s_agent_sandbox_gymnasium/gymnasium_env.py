import time

try:
    import gymnasium as gym
    from gymnasium import spaces
except ModuleNotFoundError as e:
    raise ImportError(
        "The 'gymnasium' library is required to use 'k8s_agent_sandbox.sandbox_env'. "
        "Please install the 'rl' extra: `pip install 'k8s-agent-sandbox[rl]'` "
        "or install gymnasium directly: `pip install gymnasium`."
    ) from e

from typing import Optional

from k8s_agent_sandbox import SandboxClient

from .reward_fn import RewardFn
from .termination_fn import TerminationFn


class SandboxEnv(gym.Env):
    """
    A Gymnasium-compatible environment backed by a Kubernetes Agent Sandbox.

    Each episode provisions (or claims from a warmpool) a fresh isolated pod.
    The agent submits shell commands as actions; observations are the resulting
    stdout/stderr text.

    Args:
        reward_fn:      A RewardFn instance that scores each (action, obs, info, task).
        warmpool:       Name of the SandboxWarmpool to use.
        namespace:      Kubernetes namespace.
        connection_mode: One of "tunnel" (local/KinD), "gateway" (GKE),
                         "in_cluster", or "direct".
        connection_cfg: Extra kwargs forwarded to the connection config constructor
                        (e.g. {"gateway_name": "my-gw"} or {"api_url": "http://..."}).
        max_episode_steps: Hard truncation limit per episode.
        max_obs_length:    Max characters kept from stdout/stderr.
    """

    metadata = {"render_modes": []}

    def __init__(
        self,
        reward_fn: RewardFn,
        termination_fn: TerminationFn,
        warmpool: str = "simple-sandbox-warmpool",
        namespace: str = "default",
        client: SandboxClient = None,
        max_episode_steps: int = 20,
        max_obs_length: int = 4096,
    ):
        super().__init__()

        if not isinstance(reward_fn, RewardFn):
            raise TypeError(f"reward_fn must be a RewardFn instance, got {type(reward_fn)}")

        self.reward_fn         = reward_fn
        self.termination_fn    = termination_fn
        self.warmpool          = warmpool
        self.namespace         = namespace
        self.max_episode_steps = max_episode_steps
        self.max_obs_length    = max_obs_length

        self._client        = client
        self._sandbox       = None
        self._current_task  = ""
        self._step_count    = 0

        # Gymnasium spaces — text-native; wrap with TokenizedWrapper for RLlib/SB3
        self.observation_space = spaces.Text(max_length=max_obs_length)
        self.action_space      = spaces.Text(max_length=2048)

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
            result     = self._sandbox.commands.run(action)
            stdout     = result.stdout or ""
            stderr     = result.stderr or ""
            exit_code  = result.exit_code
            env_error  = False
        except Exception as exc:
            stdout     = ""
            stderr     = str(exc)
            exit_code  = -1
            env_error  = True

        elapsed_ms = int((time.monotonic() - t0) * 1000)

        # Observation: prefer stdout, fall back to stderr
        obs = (stdout if stdout else stderr)[: self.max_obs_length]

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
            except Exception:
                pass  # best-effort cleanup
            self._sandbox = None
