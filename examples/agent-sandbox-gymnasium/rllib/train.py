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
from contextlib import ExitStack
import json
from pathlib import Path
from uuid import uuid4
from importlib.metadata import version
from typing import Any

import numpy as np
import ray
from ray.rllib.algorithms.ppo import PPOConfig
from ray.rllib.core import Columns
import torch

from file_task_env import ACTION_NAMES, SandboxFileTaskEnv
from k8s_agent_sandbox.k8s_helper import K8sHelper
from verify_sandbox_task import (
    claim_lookup, close_environment, prepare_run_directory, training_counters,
    validate_checkpoint, validate_env_evidence, validate_runner_evidence,
    verify_file_task, wait_for_claim_deletion,
)


def run_episode(algorithm, env_config, *, evidence=None):
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

        if evidence is not None:
            validate_env_evidence(env.get_claim_evidence())
        return trace, episode_return, bool(info.get("success", False))
    finally:
        close_environment(env, evidence=evidence)


def collect_runner_evidence(algorithm):
    """Query real remote actors using Ray's new-stack vector environment API."""
    def collect(runner):
        context = ray.get_runtime_context()
        return {
            "worker_index": runner.worker_index,
            "actor_id": context.get_actor_id(),
            "node_id": context.get_node_id(),
            "envs": runner.env.unwrapped.call("get_claim_evidence"),
        }
    return algorithm.env_runner_group.foreach_env_runner(
        collect, local_env_runner=False, timeout_seconds=30,
    )


def known_claims(report):
    """Keep all successful resets, including ones preceding a later failure."""
    envs = list(report["driver_envs"])
    for phase in report["runner_records"]:
        for record in phase["records"]:
            envs.extend(record.get("envs", []))
    return [claim for env in envs for claim in env.get("claims", [])
            if isinstance(claim.get("namespace"), str) and claim.get("namespace")
            and isinstance(claim.get("claim_name"), str) and claim.get("claim_name")]


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
        choices=("tunnel", "in-cluster", "sandboxd-in-cluster"),
        default="tunnel",
        help="tunnel is local; in-cluster is legacy runtime; sandboxd-in-cluster uses Service DNS.",
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
        "--verify-run", action="store_true",
        help="Require remote execution, training, checkpoint and named-Claim cleanup evidence.",
    )
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
    if args.verify_run and args.num_env_runners < 2:
        raise ValueError("--verify-run requires at least two remote EnvRunners")

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
    if args.verify_run:
        config = config.fault_tolerance(
            restart_failed_env_runners=False,
            restart_failed_sub_environments=False,
            ignore_env_runner_failures=False,
        )

    algorithm = None
    restored_algorithm = None
    run_directory = None
    lookup = None
    lookup_resources = ExitStack()
    primary_error = None
    failure_phase = None
    phase = "configuration"
    cleanup_errors = []
    report: dict[str, Any] = {
        "run_id": uuid4().hex, "driver_envs": [], "runner_records": [], "iterations": [],
    }
    driver_evidence = report["driver_envs"] if args.verify_run else None

    def emit(event, **details):
        if args.verify_run:
            print(json.dumps({"event": event, "run_id": report["run_id"], **details}), flush=True)

    def capture_runners(current, phase, *, require_execution):
        records = collect_runner_evidence(current)
        # Retain identities before validating, including evidence of a failure.
        report["runner_records"].append({"phase": phase, "records": records})
        validate_runner_evidence(records, expected_runners=args.num_env_runners,
                                 require_execution=require_execution)
        for record in records:
            summary = {**record, "envs": [
                {**env, "claims": env["claims"][:3], "claim_count": len(env["claims"])}
                for env in record["envs"]
            ]}
            emit("ENV_RUNNER_EVIDENCE", phase=phase, **summary)

    def verify_deleted(phase):
        deleted = wait_for_claim_deletion(known_claims(report), lookup)
        emit("CLAIMS_DELETED", phase=phase, count=deleted)
        report["deleted_claims"] = deleted

    try:
        if args.verify_run:
            phase = "storage-probe"
            run_directory = prepare_run_directory(args.checkpoint_dir, report["run_id"])
            phase = "kubernetes-client"
            lookup = lookup_resources.enter_context(claim_lookup(K8sHelper().custom_objects_api))
            emit("RUN_CONFIG", env_config=env_config, num_env_runners=args.num_env_runners,
                 versions={name: version(name) for name in (
                     "ray", "torch", "gymnasium", "k8s-agent-sandbox", "k8s-agent-sandbox-gymnasium",
                 )}, checkpoint_directory=str(run_directory))
        phase = "ray-init"
        if args.ray_address:
            ray.init(address=args.ray_address, log_to_driver=False)
        else:
            ray.init(num_cpus=args.num_cpus, include_dashboard=False, log_to_driver=False)

        if args.verify_run:
            phase = "preflight"
            verify_file_task(env_config, evidence=driver_evidence, verify_rest=True)
            phase = "preflight-cleanup"
            verify_deleted("preflight")
            emit("PREFLIGHT_OK", rest=True, commands=True)
        phase = "build-training-algorithm"
        algorithm = config.build_algo()

        phase = "before-training-evaluation"
        before_trace, before_return, before_success = run_episode(
            algorithm, env_config, evidence=driver_evidence
        )
        print("\nPolicy before training:", flush=True)
        print(format_trace(before_trace), flush=True)
        print(
            f"  return={before_return:+.2f} success={before_success}",
            flush=True,
        )

        for iteration in range(1, args.iterations + 1):
            phase = f"training-iteration-{iteration}"
            result = algorithm.train()
            if args.verify_run:
                counters = training_counters(result)
                report["iterations"].append({"iteration": iteration, **counters})
                emit("PPO_ITERATION", iteration=iteration, **counters,
                     episode_return_mean=episode_return_mean(result))
            if iteration == 1 or iteration % 5 == 0:
                print(
                    f"training iteration={iteration:02d} "
                    f"episode_return_mean={episode_return_mean(result)}",
                    flush=True,
                )

        phase = "after-training-evaluation"
        after_trace, after_return, after_success = run_episode(
            algorithm, env_config, evidence=driver_evidence
        )
        print("\nPolicy after training:", flush=True)
        print(format_trace(after_trace), flush=True)
        print(
            f"  return={after_return:+.2f} success={after_success}",
            flush=True,
        )

        if args.verify_run:
            phase = "training-evidence"
            capture_runners(algorithm, "training", require_execution=True)
            if run_directory is None:
                raise RuntimeError("verification run directory was not initialized")
            checkpoint_dir = run_directory / "algorithm"
        else:
            checkpoint_dir = Path(args.checkpoint_dir).resolve()
        phase = "checkpoint-save"
        checkpoint_dir.mkdir(parents=True, exist_ok=True)
        checkpoint_path = algorithm.save_to_path(checkpoint_dir)
        if args.verify_run:
            checkpoint_path = validate_checkpoint(checkpoint_path, run_directory)
            report["checkpoint_path"] = checkpoint_path
            emit("CHECKPOINT_SAVED", path=checkpoint_path)
        print(f"\nCheckpoint saved to: {checkpoint_path}", flush=True)

        # stop() requests cleanup; named GET/404 checks below prove deletion.
        phase = "stop-training-algorithm"
        algorithm.stop()
        algorithm = None
        if args.verify_run:
            phase = "training-cleanup"
            verify_deleted("training")

        phase = "build-restored-algorithm"
        restored_algorithm = config.build_algo()
        phase = "checkpoint-restore"
        restored_algorithm.restore_from_path(checkpoint_path)
        emit("CHECKPOINT_RESTORED", path=str(checkpoint_path))
        phase = "restored-evaluation"
        restored_trace, restored_return, restored_success = run_episode(
            restored_algorithm, env_config, evidence=driver_evidence
        )
        print("\nRestored policy evaluation:", flush=True)
        print(format_trace(restored_trace), flush=True)
        print(
            f"  return={restored_return:+.2f} "
            f"success={restored_success}",
            flush=True,
        )
        if args.verify_run:
            report["restored_evaluation"] = {
                "trace": restored_trace, "return": restored_return, "success": restored_success,
            }
            phase = "restored-evidence"
            capture_runners(restored_algorithm, "restored", require_execution=False)
        phase = "stop-restored-algorithm"
        restored_algorithm.stop()
        restored_algorithm = None
        if args.verify_run:
            phase = "final-cleanup"
            verify_deleted("final")
    except Exception as exc:
        primary_error = exc
        failure_phase = phase
    finally:
        for current, cleanup_phase in ((restored_algorithm, "restored-failure"), (algorithm, "training-failure")):
            if current is None:
                continue
            if args.verify_run:
                try:
                    capture_runners(current, cleanup_phase, require_execution=False)
                except Exception as exc:
                    cleanup_errors.append(f"evidence collection {cleanup_phase}: {exc}")
            try:
                current.stop()
            except Exception as exc:
                cleanup_errors.append(f"Algorithm stop {cleanup_phase}: {exc}")
        if args.verify_run and lookup is not None and primary_error is not None:
            try:
                verify_deleted("failure")
            except Exception as exc:
                cleanup_errors.append(str(exc))
        try:
            lookup_resources.close()
        except Exception as exc:
            cleanup_errors.append(f"Claim lookup client close: {exc}")
        try:
            ray.shutdown()
        except Exception as exc:
            cleanup_errors.append(f"Ray shutdown: {exc}")

    if cleanup_errors and primary_error is None:
        primary_error = RuntimeError("; ".join(cleanup_errors))
        failure_phase = "finalization"
    if args.verify_run:
        report["status"] = "FAILED" if primary_error else "SUCCEEDED"
        report["cleanup_errors"] = cleanup_errors
        if primary_error:
            report["error"] = str(primary_error)
            report["failure_phase"] = failure_phase
        if run_directory is not None:
            try:
                temporary_report = run_directory / ".verification.json.tmp"
                temporary_report.write_text(json.dumps(report, indent=2) + "\n")
                temporary_report.replace(run_directory / "verification.json")
            except Exception as exc:
                if primary_error is None:
                    primary_error = exc
                    failure_phase = "report-write"
                cleanup_errors.append(f"verification report write: {exc}")
        if primary_error:
            emit("RUN_FAILED", phase=failure_phase, error=str(primary_error), cleanup_errors=cleanup_errors,
                 known_claims=known_claims(report))
        else:
            emit("RUN_SUCCEEDED", checkpoint_path=report["checkpoint_path"],
                 deleted_claims=report["deleted_claims"],
                 report_path=str(run_directory / "verification.json") if run_directory is not None else None)
    if primary_error is not None:
        raise primary_error


if __name__ == "__main__":
    main()
