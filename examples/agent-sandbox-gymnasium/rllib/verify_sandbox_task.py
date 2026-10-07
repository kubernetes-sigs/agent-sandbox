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

"""Verify the deterministic file-task path against a real Kubernetes Sandbox."""

import argparse

from file_task_env import CREATE_DIRECTORY, SandboxFileTaskEnv, WRITE_FILE


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--warmpool", default="simple-sandbox-warmpool")
    parser.add_argument("--namespace", default="gymnasium")
    parser.add_argument(
        "--connection-mode",
        choices=("tunnel", "in-cluster"),
        default="tunnel",
    )
    parser.add_argument(
        "--router-namespace",
        default="agent-sandbox-system",
    )
    return parser.parse_args()


def main():
    args = parse_args()
    env = SandboxFileTaskEnv(
        {
            "warmpool": args.warmpool,
            "namespace": args.namespace,
            "connection_mode": args.connection_mode,
            "router_namespace": args.router_namespace,
            "max_episode_steps": 4,
        }
    )

    try:
        observation, reset_info = env.reset()
        print(
            f"claimed {reset_info.get('claim_name', 'unknown')}: "
            f"observation={observation.tolist()}"
        )

        observation, reward, terminated, truncated, info = env.step(
            CREATE_DIRECTORY
        )
        print(
            f"create-directory: observation={observation.tolist()} "
            f"reward={reward:+.2f} "
            f"sandbox_output={info['sandbox_observation']!r}"
        )
        if terminated or truncated:
            raise RuntimeError("episode ended before the file was written")

        observation, reward, terminated, truncated, info = env.step(WRITE_FILE)
        print(
            f"write-file: observation={observation.tolist()} "
            f"reward={reward:+.2f} "
            f"sandbox_output={info['sandbox_observation']!r}"
        )
        if not terminated or truncated or not info["success"]:
            raise RuntimeError("file task did not terminate successfully")
    finally:
        env.close()


if __name__ == "__main__":
    main()
