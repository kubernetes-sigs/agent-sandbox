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

"""Run a CI job inside an Agent Sandbox.

Claims one sandbox from a warm pool, runs each command in sequence, echoes
its output to this process (so the CI system captures it), and exits with the
first failing command's exit code. The sandbox is always terminated, and the
claim carries a shutdown time so the controller reclaims it even when this
process dies without cleanup.

Usage:
    python run_ci_job.py [--namespace NS] [--timeout SECS] -- "make build" "make test"
"""

import argparse
import os
import re
import sys

from k8s_agent_sandbox import SandboxClient
from kubernetes import client as k8s_client
from kubernetes import config as k8s_config

NAMESPACE = os.getenv("SANDBOX_NAMESPACE", "default")
WARMPOOL = os.getenv("SANDBOX_WARMPOOL_NAME", "ci-runner-pool")
# Optional kubeconfig context, e.g. "kind-agent-sandbox"; unset uses the
# current context.
KUBE_CONTEXT = os.getenv("SANDBOX_KUBE_CONTEXT", "")
# Per-command wall-clock limit inside the sandbox.
DEFAULT_TIMEOUT = float(os.getenv("CI_COMMAND_TIMEOUT", "1800"))
# Backstop TTL: the controller deletes the claim and its sandbox after this
# many seconds even if terminate() never runs (CI job killed, network loss).
DEFAULT_TTL = 3600
# Exit code reported when a command exceeds its timeout, matching the
# convention of `timeout(1)`.
TIMEOUT_EXIT_CODE = 124


def sanitize_claim_name(raw: str) -> str:
    """Lowercase raw and replace runs of non-alphanumeric characters with '-'.

    Returns a string safe for a DNS-1123 label (possibly empty), truncated to
    fit the "ci-" prefix within Kubernetes' 63-character limit.
    """
    cleaned = re.sub(r"[^a-z0-9-]+", "-", raw.lower()).strip("-")
    # Strip again after truncation: a cut can land on a hyphen, and the SDK's
    # DNS-1123 validation rejects names that end with one.
    return cleaned[:60].rstrip("-")


def build_claim_name(run_id: str) -> str:
    """Turn a CI run id into a traceable, DNS-safe claim name."""
    sanitized = sanitize_claim_name(run_id)
    return f"ci-{sanitized}" if sanitized else "ci-job"


def build_client() -> SandboxClient:
    if KUBE_CONTEXT:
        # Pin the control plane (claims) and the data plane (the tunnel's
        # kubectl port-forward reuses this api_client) to one cluster.
        kube_config = k8s_config.load_kube_config(context=KUBE_CONTEXT)
        return SandboxClient(api_client=k8s_client.ApiClient(configuration=kube_config))
    return SandboxClient()


def run_commands(sandbox, commands: list[str], timeout: float) -> int:
    """Run commands in order; return the first failure's exit code, else 0."""
    for i, command in enumerate(commands, start=1):
        print(f"=== [{i}/{len(commands)}] {command} ===", flush=True)
        result = sandbox.commands.run(command, command_timeout=timeout)
        if result.stdout:
            print(result.stdout, flush=True)
        if result.timed_out:
            print(
                f"command timed out after {timeout}s",
                file=sys.stderr,
                flush=True,
            )
            return TIMEOUT_EXIT_CODE
        if result.stderr:
            print(result.stderr, file=sys.stderr, flush=True)
        if result.exit_code != 0:
            return result.exit_code
    return 0


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run CI commands in a sandbox claimed from a warm pool.",
    )
    parser.add_argument("--namespace", default=NAMESPACE)
    parser.add_argument("--warmpool", default=WARMPOOL)
    parser.add_argument(
        "--timeout",
        type=float,
        default=DEFAULT_TIMEOUT,
        help="per-command timeout in seconds (default: %(default)s)",
    )
    parser.add_argument(
        "--ttl",
        type=int,
        default=DEFAULT_TTL,
        help="backstop lifetime for the claim in seconds (default: %(default)s)",
    )
    parser.add_argument(
        "--claim-name",
        default=None,
        help="run id used to name the claim (default: $GITHUB_RUN_ID, else 'ci-job')",
    )
    parser.add_argument(
        "commands",
        nargs=argparse.REMAINDER,
        help="commands to run, after '--'",
    )
    args = parser.parse_args(argv)
    if args.commands and args.commands[0] == "--":
        args.commands = args.commands[1:]
    if not args.commands:
        parser.error("no commands given; pass them after '--'")
    return args


def main(argv: list[str] | None = None, client_factory=build_client) -> int:
    args = parse_args(argv)
    claim_name = build_claim_name(
        args.claim_name or os.getenv("GITHUB_RUN_ID", "")
    )
    client = client_factory()
    print(
        f"Claiming sandbox from warmpool {args.warmpool!r} "
        f"in namespace {args.namespace!r} as {claim_name!r}...",
        flush=True,
    )
    sandbox = client.create_sandbox(
        warmpool=args.warmpool,
        namespace=args.namespace,
        claim_name=claim_name,
        shutdown_after_seconds=args.ttl,
    )
    try:
        return run_commands(sandbox, args.commands, args.timeout)
    finally:
        sandbox.terminate()


if __name__ == "__main__":
    sys.exit(main())
