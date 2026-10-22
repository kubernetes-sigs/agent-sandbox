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

"""Drive Claude Code headlessly inside an Agent Sandbox.

Claims a sandbox from the claude-code-pool warm pool and runs three stages:
a one-shot answer, seeding a memory, then resuming the same session to prove
multi-turn continuity via `claude --resume <session_id>`.
"""

import json
import os
import shlex
import sys

from k8s_agent_sandbox import SandboxClient
from kubernetes import client as k8s_client
from kubernetes import config as k8s_config

NAMESPACE = os.getenv("SANDBOX_NAMESPACE", "default")
WARMPOOL = os.getenv("SANDBOX_WARMPOOL_NAME", "claude-code-pool")
# Optional kubeconfig context, e.g. "kind-agent-sandbox". Keep this in sync
# with the cluster run-test-kind.sh deploys to; unset uses the current context.
KUBE_CONTEXT = os.getenv("SANDBOX_KUBE_CONTEXT", "")
# LLM turns are slow; keep under the sandbox-router --proxy-timeout (180s).
COMMAND_TIMEOUT = 120.0


def run_claude(sandbox, prompt: str, extra_args: str = "") -> dict:
    """Run `claude -p <prompt> --output-format json` and parse the result."""
    cmd = (
        f"claude -p {shlex.quote(prompt)} --output-format json"
        + (f" {extra_args}" if extra_args else "")
    )
    result = sandbox.commands.run(cmd, command_timeout=COMMAND_TIMEOUT)
    if result.timed_out:
        sys.exit(f"claude command timed out after {COMMAND_TIMEOUT}s")
    if result.exit_code != 0:
        print(f"[stderr] {result.stderr}", file=sys.stderr)
        sys.exit(f"claude exited with code {result.exit_code}")
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError:
        print(f"[warn] expected JSON output, got: {result.stdout[:200]!r}")
        sys.exit(1)


def main() -> None:
    if KUBE_CONTEXT:
        # Pin the control plane (claims) and the data plane (the tunnel's
        # kubectl port-forward reuses this api_client) to one cluster.
        kube_config = k8s_config.load_kube_config(context=KUBE_CONTEXT)
        client = SandboxClient(
            api_client=k8s_client.ApiClient(configuration=kube_config)
        )
    else:
        client = SandboxClient()
    print(f"Claiming a sandbox from warmpool {WARMPOOL!r} in {NAMESPACE!r}...")
    sandbox = client.create_sandbox(warmpool=WARMPOOL, namespace=NAMESPACE)
    try:
        print("\n=== stage 1: one-shot question ===")
        out = run_claude(sandbox, "Compute 1+1=?. Reply with just the number.")
        print(f"[result] {out.get('result', '')}")

        print("\n=== stage 2: seed a memory (captures session_id) ===")
        out = run_claude(
            sandbox,
            "Remember this for later: my favorite sandbox number is 42.",
        )
        session_id = out.get("session_id", "")
        if not session_id:
            sys.exit("no session_id in claude JSON output")
        print(f"[session_id] {session_id}")

        print("\n=== stage 3: resume the session ===")
        out = run_claude(
            sandbox,
            "What is my favorite sandbox number? Reply with just the number.",
            extra_args=f"--resume {shlex.quote(session_id)}",
        )
        answer = out.get("result", "")
        print(f"[result] {answer}")
        # Exact match (after trimming whitespace and a trailing period) so a
        # wrong recall like "142" cannot slip through a substring check.
        if answer.strip().rstrip(".") != "42":
            sys.exit(f"resume verification failed: expected 42, got {answer!r}")
        print("\nSUCCESS: session resumed across headless invocations.")
    finally:
        sandbox.terminate()


if __name__ == "__main__":
    main()
