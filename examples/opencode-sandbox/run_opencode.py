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

"""Drive OpenCode headlessly inside an Agent Sandbox.

Claims a sandbox from the opencode-pool warm pool and runs one
non-interactive `opencode run` turn with a simple arithmetic prompt. The
default model is the free OpenCode Zen offering; set OPENCODE_MODEL (and the
matching API key in the opencode-api-keys Secret) to use an authenticated
model instead. Do not send confidential code to the free model.
"""

import os
import shlex
import sys

from k8s_agent_sandbox import SandboxClient
from kubernetes import client as k8s_client
from kubernetes import config as k8s_config

NAMESPACE = os.getenv("SANDBOX_NAMESPACE", "default")
WARMPOOL = os.getenv("SANDBOX_WARMPOOL_NAME", "opencode-pool")
# Optional kubeconfig context, e.g. "kind-agent-sandbox". Keep this in sync
# with the cluster run-test-kind.sh deploys to; unset uses the current context.
KUBE_CONTEXT = os.getenv("SANDBOX_KUBE_CONTEXT", "")
# Model ID in provider/model format for `opencode run --model`.
OPENCODE_MODEL = os.getenv("OPENCODE_MODEL", "opencode/deepseek-v4-flash-free")
# LLM turns are slow; keep under the sandbox-router --proxy-timeout (180s).
COMMAND_TIMEOUT = 120.0


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
        cmd = (
            "cd /workspace && "
            f"opencode run --model {shlex.quote(OPENCODE_MODEL)} "
            '"Compute 1+1 and reply with only the final number."'
        )
        result = sandbox.commands.run(cmd, command_timeout=COMMAND_TIMEOUT)
        if result.timed_out:
            sys.exit(f"opencode command timed out after {COMMAND_TIMEOUT}s")
        if result.stdout:
            print(f"[stdout] {result.stdout}")
        if result.stderr:
            print(f"[stderr] {result.stderr}", file=sys.stderr)
        if result.exit_code != 0:
            sys.exit(f"opencode exited with code {result.exit_code}")
        print("\nSUCCESS: opencode run completed inside the sandbox.")
    finally:
        sandbox.terminate()


if __name__ == "__main__":
    main()
