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
from contextlib import contextmanager
from copy import deepcopy
import math
import time
import sys
from pathlib import Path

from kubernetes import client
from kubernetes.client.exceptions import ApiException
from k8s_agent_sandbox.constants import CLAIM_API_GROUP, CLAIM_API_VERSION, CLAIM_PLURAL_NAME

from file_task_env import CREATE_DIRECTORY, SandboxFileTaskEnv, WRITE_FILE


def _identifier(value):
    return isinstance(value, str) and value.strip().lower() not in (
        "", "none", "unknown", "null",
    )


def validate_env_evidence(evidence, *, require_execution=True):
    """Reject missing identities and task errors, not an unsuccessful policy."""
    claims = evidence.get("claims", [])
    if not _identifier(evidence.get("env_id")):
        raise RuntimeError("environment evidence has no instance ID")
    if evidence.get("errors", 0) != 0:
        raise RuntimeError(f"environment {evidence['env_id']} had execution errors")
    if require_execution and (not claims or evidence.get("successful_steps", 0) <= 0):
        raise RuntimeError(f"environment {evidence['env_id']} did not execute a task")
    for claim in claims:
        if not all(_identifier(claim.get(key)) for key in (
            "namespace", "claim_name", "sandbox_id",
        )):
            raise RuntimeError(f"incomplete Claim identity: {claim}")
    return claims


def validate_runner_evidence(records, *, expected_runners, require_execution=True):
    """Prove every remote runner used its own environment and Sandboxes."""
    if len(records) != expected_runners:
        raise RuntimeError(f"expected {expected_runners} remote EnvRunners, got {len(records)}")
    actors, workers, environments = set(), set(), set()
    claim_names: set[tuple[str, str]] = set()
    sandbox_ids: set[tuple[str, str]] = set()
    claims = []
    for record in records:
        worker = record.get("worker_index")
        actor = record.get("actor_id")
        if not isinstance(worker, int) or worker <= 0 or worker in workers:
            raise RuntimeError("remote EnvRunner worker_index must be distinct and positive")
        if not _identifier(actor) or actor in actors:
            raise RuntimeError("remote EnvRunner actor_id must be distinct and nonempty")
        workers.add(worker)
        actors.add(actor)
        envs = record.get("envs", [])
        if len(envs) != 1:
            raise RuntimeError("each remote EnvRunner must have exactly one environment")
        evidence = envs[0]
        runner_claims = validate_env_evidence(evidence, require_execution=require_execution)
        if evidence["env_id"] in environments:
            raise RuntimeError("remote EnvRunners share an environment")
        environments.add(evidence["env_id"])
        own_claims = {(claim["namespace"], claim["claim_name"]) for claim in runner_claims}
        own_sandboxes = {(claim["namespace"], claim["sandbox_id"]) for claim in runner_claims}
        if own_claims & claim_names or own_sandboxes & sandbox_ids:
            raise RuntimeError("remote EnvRunners share Claim or Sandbox identities")
        claim_names.update(own_claims)
        sandbox_ids.update(own_sandboxes)
        claims.extend(runner_claims)
    return claims


def training_counters(result):
    """Require Ray 2.58 sampling and learner counters, with no success fallback."""
    counters = {
        "sampled": result.get("env_runners", {}).get("num_env_steps_sampled_lifetime"),
        "trained": result.get("learners", {}).get("__all_modules__", {}).get("num_env_steps_trained_lifetime"),
    }
    for name, value in counters.items():
        if not isinstance(value, (int, float)) or isinstance(value, bool) or not math.isfinite(value) or value <= 0:
            raise RuntimeError(f"missing or invalid {name} training counter: {value!r}")
    return counters


@contextmanager
def claim_lookup(custom_objects_api):
    """Own a non-retrying GET client without changing SDK request behavior."""
    source_client = custom_objects_api.api_client
    configuration = deepcopy(source_client.configuration)
    # Retries reuse each attempt's timeout and can exceed the shared deadline.
    configuration.retries = 0
    with client.ApiClient(configuration=configuration, cookie=source_client.cookie) as api_client:
        api_client.default_headers = dict(source_client.default_headers)
        api = client.CustomObjectsApi(api_client)

        def lookup(namespace, name, request_timeout):
            # Split the remaining request budget between connect and read phases.
            per_phase = min(5.0, request_timeout / 2)
            return api.get_namespaced_custom_object(
                group=CLAIM_API_GROUP, version=CLAIM_API_VERSION,
                plural=CLAIM_PLURAL_NAME, namespace=namespace, name=name,
                _request_timeout=(per_phase, per_phase),
            )

        try:
            yield lookup
        finally:
            # ApiClient.close() only closes its async pool. Error tracebacks
            # can retain HTTP pools even after PoolManager.clear().
            pool_manager = api_client.rest_client.pool_manager
            try:
                pool_manager.connection_from_url(configuration.host).close()
            finally:
                pool_manager.clear()


def wait_for_claim_deletion(claims, lookup, *, timeout_seconds=120,
                            monotonic=time.monotonic, sleep=time.sleep):
    """Verify only this run's known Claims; 404, not absence by assumption."""
    pending = {(claim["namespace"], claim["claim_name"]) for claim in claims}
    count = len(pending)
    deadline = monotonic() + timeout_seconds
    reason = "deletion deadline expired"
    while pending and monotonic() < deadline:
        for namespace, name in sorted(pending):
            remaining = deadline - monotonic()
            if remaining <= 0:
                break
            try:
                lookup(namespace, name, remaining)
            except ApiException as exc:
                if exc.status == 404:
                    pending.remove((namespace, name))
                    continue
                reason = f"Claim GET failed: HTTP {exc.status}"
                break
            except Exception as exc:
                reason = f"Claim GET failed: {type(exc).__name__}: {exc}"
                break
        else:
            if pending:
                sleep(min(1.0, max(0.0, deadline - monotonic())))
            continue
        break
    if pending:
        names = ", ".join(f"{namespace}/{name}" for namespace, name in sorted(pending))
        commands = "; ".join(
            f"kubectl --context YOUR_VERIFIED_CONTEXT -n {namespace} delete sandboxclaim {name}"
            for namespace, name in sorted(pending)
        )
        raise RuntimeError(f"{reason}; remaining Claims: {names}. Inspect before cleanup: {commands}")
    return count


def prepare_run_directory(checkpoint_root, run_id):
    """Probe writable storage before training; never overwrite another run."""
    directory = Path(checkpoint_root).resolve() / run_id
    directory.mkdir(parents=True, exist_ok=False)
    probe = directory / ".write-probe"
    probe.write_text(run_id)
    if probe.read_text() != run_id:
        raise RuntimeError("checkpoint storage write/read probe failed")
    probe.unlink()
    return directory


def validate_checkpoint(checkpoint_path, run_directory):
    """Require real checkpoint files beneath this run's storage directory."""
    path = Path(checkpoint_path).resolve()
    if not path.is_relative_to(Path(run_directory).resolve()) or not path.is_dir() or not any(
        entry.is_file() for entry in path.rglob("*")
    ):
        raise RuntimeError(f"checkpoint is missing or outside the run directory: {path}")
    return str(path)


def close_environment(env, *, evidence=None):
    """Retain identities and close without replacing an active task error."""
    primary_error = sys.exception()
    if evidence is not None:
        evidence.append(env.get_claim_evidence())
    try:
        env.close()
    except Exception as close_error:
        if primary_error is not None:
            raise primary_error from close_error
        raise


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--warmpool", default="simple-sandbox-warmpool")
    parser.add_argument("--namespace", default="gymnasium")
    parser.add_argument(
        "--connection-mode",
        choices=("tunnel", "in-cluster", "sandboxd-in-cluster"),
        default="tunnel",
        help="in-cluster uses the legacy runtime; sandboxd-in-cluster uses Service DNS.",
    )
    parser.add_argument(
        "--router-namespace",
        default="agent-sandbox-system",
    )
    return parser.parse_args()


def verify_file_task(env_config, *, evidence=None, verify_rest=False):
    """Run the deterministic command task and optional REST round trip."""
    env = SandboxFileTaskEnv(env_config)

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
        validate_env_evidence(env.get_claim_evidence())
        if verify_rest:
            env.verify_rest_file()
    finally:
        close_environment(env, evidence=evidence)


def main():
    args = parse_args()
    verify_file_task({
        "warmpool": args.warmpool,
        "namespace": args.namespace,
        "connection_mode": args.connection_mode,
        "router_namespace": args.router_namespace,
        "max_episode_steps": 4,
    }, verify_rest=args.connection_mode == "sandboxd-in-cluster")


if __name__ == "__main__":
    main()
