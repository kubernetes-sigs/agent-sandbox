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

"""DeepAgents + agent-sandbox example.

Runs a Deep Agent whose file and shell tools execute inside an Agent Sandbox
pod. The deepagents-k8s-agent-sandbox integration adapts the k8s-agent-sandbox
Python SDK to the Deep Agents BaseSandbox interface, so every file read/write
and command the agent runs is sandboxed.

Prerequisites:
    1. A cluster with the agent-sandbox controller and extension controllers
       (EXTENSIONS=true make deploy-kind), KUBECONFIG exported.
    2. The example manifests applied:
         kubectl apply -f sandbox-template.yaml -f warmpool.yaml
    3. Dependencies installed from this directory:
         pip install -r requirements.txt
    4. ANTHROPIC_API_KEY exported (used by the default Deep Agents model).

Run from this directory:

    python main.py

Environment:
    AGENT_SANDBOX_NAMESPACE  Namespace of the template/warmpool. Default "default".
    AGENT_SANDBOX_WARMPOOL   WarmPool name. Default "deepagents-warmpool".
    ANTHROPIC_API_KEY        Required by the default Deep Agents model.
    DEEPAGENTS_MODEL         Optional model string passed to create_deep_agent
                             (e.g. "anthropic:claude-sonnet-4-5"). Unset uses
                             the Deep Agents default model.
"""

import os
import sys

from deepagents import create_deep_agent
from deepagents_k8s_agent_sandbox import K8sAgentSandbox
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig

NAMESPACE = os.getenv("AGENT_SANDBOX_NAMESPACE", "default")
WARMPOOL = os.getenv("AGENT_SANDBOX_WARMPOOL", "deepagents-warmpool")
MODEL = os.getenv("DEEPAGENTS_MODEL")

PROMPT = (
    "Write a script that prints the first 10 Fibonacci numbers, save it as "
    "fib.py, run it, and report the output."
)
# The first 10 Fibonacci numbers as tokens. The agent may print them
# comma-separated, one per line, or with a leading caption, so the check
# below looks for this sequence among whitespace/comma-split tokens.
FIB_TOKENS = ["0", "1", "1", "2", "3", "5", "8", "13", "21", "34"]


def _contains_sequence(tokens: list[str], expected: list[str]) -> bool:
    """Return True if expected appears in tokens in order (not necessarily contiguous)."""
    it = iter(tokens)
    return all(any(token == want for token in it) for want in expected)
# Server-side TTL backstop: if this process is killed before the finally
# block runs (SIGTERM does not unwind Python), the controller still deletes
# the claim after this many seconds.
SHUTDOWN_AFTER_SECONDS = 1800
# Marks claims created by this script so cleanup can select exactly them
# (see the README troubleshooting section).
CLAIM_LABELS = {"deepagents.example/claim": "true"}


def main() -> None:
    # Connect straight to sandboxd on the pod (REST 8080 / gRPC 9090) via
    # port-forward, so the example needs no sandbox-router deployment.
    client = SandboxClient(connection_config=SandboxdPodTunnelConnectionConfig())
    # One-shot claim from the warm pool; deleted in the finally block so a
    # failed agent run does not leak sandboxes.
    sandbox = client.create_sandbox(
        warmpool=WARMPOOL,
        namespace=NAMESPACE,
        shutdown_after_seconds=SHUTDOWN_AFTER_SECONDS,
        labels=CLAIM_LABELS,
    )
    try:
        # Defaults match the template: sandbox_api_cwd="/app" (sandboxd's
        # --root-dir) and backend working directory "/app/work".
        backend = K8sAgentSandbox.from_existing_sandbox(sandbox)
        # model=None (DEEPAGENTS_MODEL unset) selects the Deep Agents default.
        agent = create_deep_agent(
            model=MODEL,
            tools=[],
            system_prompt=(
                "You are a coding assistant. Use the sandbox to write and run "
                "Python, and verify your work by executing it."
            ),
            backend=backend,
        )
        result = agent.invoke(
            {"messages": [{"role": "user", "content": PROMPT}]}
        )
        answer = result["messages"][-1].content
        print(answer)
        # Verify against the sandbox itself instead of trusting the agent's
        # prose: fib.py must run in the pod and print the expected sequence.
        check = sandbox.commands.run("sh -c 'cd /app/work && python3 fib.py'")
        tokens = [token.strip(",") for token in check.stdout.split()]
        if check.exit_code != 0 or not _contains_sequence(tokens, FIB_TOKENS):
            sys.exit(
                "fib.py did not produce the expected output inside the sandbox "
                f"(exit_code={check.exit_code}, stdout={check.stdout!r})"
            )
        print(
            "Sandbox verification passed: fib.py runs in the pod and prints "
            "the expected sequence."
        )
    finally:
        # terminate() closes the client-side connection and deletes the
        # SandboxClaim, which tears down the sandbox pod. Unlike
        # client.delete_sandbox(), it propagates deletion failures instead of
        # logging and swallowing them, and it is idempotent.
        sandbox.terminate()


if __name__ == "__main__":
    main()
