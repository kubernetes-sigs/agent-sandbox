# Kubernetes Agent Sandbox Gymnasium Environment

This integration provides a standard [Gymnasium](https://gymnasium.farama.org/) (`gym.Env`) environment backed by the Kubernetes Agent Sandbox. It enables Reinforcement Learning (RL) agents or custom evaluation loops to provision, execute, and evaluate interactions seamlessly within secure, isolated Kubernetes sandboxes.

## Features

- **Standard `gym.Env` interface**: Fits right into standard RL pipelines.
- **Isolated Pods per Episode**: Each episode automatically claims a fresh sandbox from a Kubernetes warmpool and tears it down gracefully when `env.reset()` is called. When the episode ends you should invoke `env.reset()` to get a fresh environment.
- **Text-native Spaces**: The environment features text-based `observation_space` (stdout/stderr) and `action_space` (shell commands).
- **Customizable Rewards and Terminations**: Extend `RewardFn` and `TerminationFn` classes to precisely define your scoring and episodic completion logic.
- **Support for Multi-Connection Backends**: Leverage any configured k8s-agent-sandbox backend ("tunnel", "gateway", "in_cluster", or "direct") for robust scaling.

## Quick Start

### Installation

Install the Gymnasium integration directly from pip (once published) or via a local editable install within the repo:

```bash
pip install k8s-agent-sandbox-gymnasium
```

> **Note**: You will also need a running Kubernetes cluster with the Sandbox controller and warmpool deployed.

### Basic Usage

```python
from k8s_agent_sandbox_gymnasium.gymnasium_env import SandboxEnv
from k8s_agent_sandbox_gymnasium.reward_fns import ExitCodeReward
from k8s_agent_sandbox_gymnasium.termination_fns import SparseTaskTermination

# 1. Define rewards and termination logic
reward_fn = ExitCodeReward()  # +1 for success (exit code 0), -1 for failure
termination_fn = SparseTaskTermination(success_fn=lambda obs, info: info.get("exit_code") == 0)

# 2. Instantiate the SandboxEnv
env = SandboxEnv(
    reward_fn=reward_fn,
    termination_fn=termination_fn,
    client=SandboxClient(...), # SandboxClient definition pseudocode
    warmpool="simple-sandbox-warmpool",
    namespace="default",
    max_episode_steps=10
)

# 3. Standard Gymnasium loop
obs, info = env.reset(options={"task": "Create a file named hello.txt"})
print(f"Initial Observation: {obs}")

action = "echo 'Hello, World!' > hello.txt"
obs, reward, terminated, truncated, info = env.step(action)

print(f"Action: {action}")
print(f"Observation: {obs}")
print(f"Reward: {reward}")
print(f"Terminated: {terminated}")

env.close()
```

## Creating Custom Rewards and Terminations

The integration ships with several useful built-in reward and termination classes in `k8s_agent_sandbox_gymnasium.reward_fns` and `k8s_agent_sandbox_gymnasium.termination_fns`. 

To define your own custom behavior, extend the base `RewardFn` or `TerminationFn` classes:

```python
from k8s_agent_sandbox_gymnasium.reward_fn import RewardFn
from k8s_agent_sandbox_gymnasium.termination_fn import TerminationFn

class MyCustomReward(RewardFn):
    def reset(self, task: str):
        # Optional: initialize/reset any state tracking at the start of each episode
        self.step_count = 0

    def __call__(self, action: str, obs: str, info: dict, task: str) -> float:
        self.step_count += 1
        if "success" in obs.lower() and info.get("exit_code") == 0:
            return 10.0
        return -0.1  # small penalty per step

class MyCustomTermination(TerminationFn):
    def __call__(self, obs: str, info: dict, task: str) -> bool:
        # Terminate when the command fails
        if info.get("exit_code", 0) != 0:
            return True
        return False
```

## Observation and Action Details

Both actions and observations map directly to shell-based inputs and outputs.

- **Action Space**: `gymnasium.spaces.Text(max_length=2048)`.
  The agent submits arbitrary shell commands as strings (e.g., `ls -la`, `python script.py`).
- **Observation Space**: `gymnasium.spaces.Text(max_length=4096)` (configurable).
  The output of the executed command. By default, it concatenates `stdout` and `stderr`. `stderr` is marked with `[stderr]` tag.

### `info` Dictionary

At every `step()`, the environment returns an `info` dictionary containing rich execution metadata:

- `exit_code` (int): The shell exit code of the command.
- `stdout` (str): Full standard output.
- `stderr` (str): Full standard error.
- `elapsed_ms` (int): Command execution time in milliseconds.
- `step` (int): The current step index in the episode.
- `env_error` (bool): `True` if the integration lost connection or the sandbox errored unexpectedly out-of-band.
