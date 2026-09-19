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

"""Train and evaluate an RLlib PPO policy against real Agent Sandboxes."""

import argparse
from pathlib import Path

import numpy as np
import ray
from ray.rllib.algorithms.ppo import PPOConfig
from ray.rllib.core import Columns
import torch

from file_task_env import ACTION_NAMES, SandboxFileTaskEnv


def run_episode(algorithm, env_config):
    """Evaluate one deterministic episode and return a readable trace."""
    env = SandboxFileTaskEnv(env_config)
    trace = []
    episode_return = 0.0

    try:
        module = algorithm.get_module()
        observation, reset_info = env.reset()
        terminated = False
        truncated = False

        while not terminated and not truncated:
            with torch.no_grad():
                module_output = module.forward_inference(
                    {
                        Columns.OBS: torch.from_numpy(
                            np.asarray([observation], dtype=np.float32)
                        )
                    }
                )
            # Evaluation is deterministic: choose the highest-scoring action.
            action = int(
                torch.argmax(
                    module_output[Columns.ACTION_DIST_INPUTS],
                    dim=-1,
                )[0]
            )
            next_observation, reward, terminated, truncated, info = env.step(
                action
            )
            trace.append(
                {
                    "observation": observation.tolist(),
                    "action": ACTION_NAMES[action],
                    "reward": reward,
                    "next_observation": next_observation.tolist(),
                    "claim_name": reset_info.get("claim_name", "unknown"),
                }
            )
            episode_return += reward
            observation = next_observation

        return trace, episode_return, bool(info.get("success", False))
    finally:
        env.close()


def format_trace(trace):
    """Format an episode trace for terminal output."""
    return "\n".join(
        f"  obs={step['observation']}  "
        f"action={step['action']:<16}  "
        f"reward={step['reward']:+.2f}  "
        f"next_obs={step['next_observation']}  "
        f"claim={step['claim_name']}"
        for step in trace
    )


def episode_return_mean(result):
    """Read the new-stack metric while tolerating minor RLlib key changes."""
    env_runner_metrics = result.get("env_runners", {})
    value = env_runner_metrics.get("episode_return_mean")
    if value is not None:
        return value
    return result.get("episode_reward_mean")


def parse_args():
    parser = argparse.ArgumentParser(
        description="Train RLlib PPO on a file task executed by SandboxEnv."
    )
    parser.add_argument(
        "--warmpool",
        default="simple-sandbox-warmpool",
        help="SandboxWarmPool claimed by every environment.",
    )
    parser.add_argument("--namespace", default="gymnasium")
    parser.add_argument(
        "--connection-mode",
        choices=("tunnel", "in-cluster"),
        default="tunnel",
        help="Use tunnel locally and in-cluster from a RayCluster or RayJob.",
    )
    parser.add_argument(
        "--router-namespace",
        default="agent-sandbox-system",
        help="Namespace of the Sandbox Router when using tunnel mode.",
    )
    parser.add_argument("--num-env-runners", type=int, default=2)
    parser.add_argument("--iterations", type=int, default=5)
    parser.add_argument("--ray-address", default=None)
    parser.add_argument("--num-cpus", type=int, default=4)
    parser.add_argument(
        "--checkpoint-dir",
        default="bin/rllib-sandbox-file-task",
    )
    return parser.parse_args()


def main():
    args = parse_args()
    if args.num_env_runners < 0:
        raise ValueError("--num-env-runners must be zero or greater")
    if args.iterations < 1:
        raise ValueError("--iterations must be at least one")

    if args.ray_address:
        ray.init(address=args.ray_address, log_to_driver=False)
    else:
        ray.init(
            num_cpus=args.num_cpus,
            include_dashboard=False,
            log_to_driver=False,
        )

    env_config = {
        "warmpool": args.warmpool,
        "namespace": args.namespace,
        "connection_mode": args.connection_mode,
        "router_namespace": args.router_namespace,
        "max_episode_steps": 4,
        "step_timeout_seconds": 60,
    }
    # env_config contains only serializable values because remote EnvRunners
    # construct their Sandbox clients inside SandboxFileTaskEnv.
    config = (
        PPOConfig()
        .environment(env=SandboxFileTaskEnv, env_config=env_config)
        .framework("torch")
        .env_runners(
            num_env_runners=args.num_env_runners,
            num_envs_per_env_runner=1,
            rollout_fragment_length=16,
            # Provisioning a real Sandbox can exceed RLlib's short default
            # sampling timeout, especially when the warm pool is replenishing.
            sample_timeout_s=300,
        )
        .learners(num_learners=0, num_gpus_per_learner=0)
        .training(
            gamma=0.95,
            lr=5e-4,
            train_batch_size_per_learner=64,
            minibatch_size=32,
            num_epochs=6,
        )
        .debugging(seed=7)
    )

    algorithm = None
    restored_algorithm = None
    try:
        algorithm = config.build_algo()

        before_trace, before_return, before_success = run_episode(
            algorithm, env_config
        )
        print("\nPolicy before training:", flush=True)
        print(format_trace(before_trace), flush=True)
        print(
            f"  return={before_return:+.2f} success={before_success}",
            flush=True,
        )

        for iteration in range(1, args.iterations + 1):
            result = algorithm.train()
            if iteration == 1 or iteration % 5 == 0:
                print(
                    f"training iteration={iteration:02d} "
                    f"episode_return_mean={episode_return_mean(result)}",
                    flush=True,
                )

        after_trace, after_return, after_success = run_episode(
            algorithm, env_config
        )
        print("\nPolicy after training:", flush=True)
        print(format_trace(after_trace), flush=True)
        print(
            f"  return={after_return:+.2f} success={after_success}",
            flush=True,
        )

        checkpoint_dir = Path(args.checkpoint_dir).resolve()
        checkpoint_dir.mkdir(parents=True, exist_ok=True)
        checkpoint_path = algorithm.save_to_path(checkpoint_dir)
        print(f"\nCheckpoint saved to: {checkpoint_path}", flush=True)

        # Stopping the original Algorithm proves its EnvRunners release their
        # SandboxClaims before a separate Algorithm restores the checkpoint.
        algorithm.stop()
        algorithm = None

        restored_algorithm = config.build_algo()
        restored_algorithm.restore_from_path(checkpoint_path)
        restored_trace, restored_return, restored_success = run_episode(
            restored_algorithm, env_config
        )
        print("\nRestored policy evaluation:", flush=True)
        print(format_trace(restored_trace), flush=True)
        print(
            f"  return={restored_return:+.2f} "
            f"success={restored_success}",
            flush=True,
        )
    finally:
        try:
            if restored_algorithm is not None:
                restored_algorithm.stop()
        finally:
            try:
                if algorithm is not None:
                    algorithm.stop()
            finally:
                ray.shutdown()


if __name__ == "__main__":
    main()
