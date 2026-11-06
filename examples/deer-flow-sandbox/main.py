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

"""DeerFlow + agent-sandbox example.

Drives AgentSandboxProvider directly -- no model, no gateway -- so the sandbox
contract can be exercised end to end. Requires:

  1. A cluster with the agent-sandbox controller and extension controllers
     (EXTENSIONS=true make deploy-kind), KUBECONFIG exported.
  2. The example manifests applied:
       kubectl apply -f sandbox-template.yaml -f warmpool.yaml
  3. A deer-flow checkout with the harness installed:
       git clone https://github.com/bytedance/deer-flow.git
       cd deer-flow/backend && uv sync --all-packages
  4. The Python SDK (with gRPC for sandboxd) in the same env:
       uv pip install "k8s-agent-sandbox[grpc]"

Run from this directory:

    uv run --project /path/to/deer-flow/backend python main.py

Environment:
    AGENT_SANDBOX_NAMESPACE  Namespace of the template/warmpool. Default "default".
    AGENT_SANDBOX_WARMPOOL   WarmPool name. Default "deer-flow-warmpool".
"""

import os
import tempfile
from pathlib import Path

WORKSPACE = "/mnt/user-data/workspace"
FIB_SCRIPT = f"{WORKSPACE}/fib.py"
FIB_SOURCE = """\
def fibonacci(count):
    first, second = 0, 1
    for _ in range(count):
        yield first
        first, second = second, first + second


print(list(fibonacci(10)))
"""
FIB_EXPECTED = "[0, 1, 1, 2, 3, 5, 8, 13, 21, 34]"

# DeerFlow's AppConfig requires only the `sandbox` section; everything else
# defaults, so a provider-only run needs no model or gateway settings.
# Cluster coordinates come from the environment (see provider.py) because
# DeerFlow's config schema owns the keys below.
CONFIG_TEMPLATE = """\
sandbox:
  use: agent_sandbox_provider.provider:AgentSandboxProvider
  image: python:3.11
  ready_timeout: 300
  sandbox_timeout: 600
  idle_timeout: 0
"""


def exercise_sandbox(sandbox) -> None:
    """Walk the DeerFlow sandbox surface the agent's tools call into."""
    print(f"[create] DeerFlow sandbox {sandbox.id}")

    platform_info = sandbox.execute_command("python3 -c 'import platform; print(platform.platform())'")
    print(f"[command] {platform_info.strip()}")

    sandbox.write_file(FIB_SCRIPT, FIB_SOURCE)
    print(f"[file] Wrote {FIB_SCRIPT}")

    output = sandbox.execute_command(f"python3 {FIB_SCRIPT}")
    print(f"[command] {output.strip()}")
    if FIB_EXPECTED not in output:
        raise RuntimeError(f"Unexpected output from {FIB_SCRIPT}: {output!r}")

    print(f"[file] Read back: {sandbox.read_file(FIB_SCRIPT, start_line=1, end_line=1).strip()}")
    print(f"[list_dir] {sandbox.list_dir(WORKSPACE)}")

    matches, _ = sandbox.glob("/mnt/user-data", "*.py")
    print(f"[glob] {matches}")
    if FIB_SCRIPT not in matches:
        raise RuntimeError(f"glob did not return {FIB_SCRIPT}: {matches!r}")

    # Full-path patterns are part of the harness contract (search.py
    # path_matches); a basename-only implementation returns nothing here.
    deep, _ = sandbox.glob("/mnt/user-data", "**/*.py")
    if FIB_SCRIPT not in deep:
        raise RuntimeError(f"glob('**/*.py') did not return {FIB_SCRIPT}: {deep!r}")

    hits, _ = sandbox.grep(WORKSPACE, "fibonacci", literal=True)
    if not hits:
        raise RuntimeError("grep found no match for 'fibonacci' in the sandbox")
    print(f"[grep] {hits[0].path}:{hits[0].line_number}: {hits[0].line.strip()}")

    download = sandbox.download_file(FIB_SCRIPT)
    print(f"[download] {len(download)} bytes fetched from {FIB_SCRIPT}")
    if len(download) != len(FIB_SOURCE.encode("utf-8")):
        raise RuntimeError(f"download size mismatch: {len(download)} bytes")


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="agent-sandbox-deerflow-") as tmp:
        config_path = Path(tmp) / "config.yaml"
        config_path.write_text(CONFIG_TEMPLATE, encoding="utf-8")
        # The provider reads this when it is constructed, below.
        os.environ["DEER_FLOW_CONFIG_PATH"] = str(config_path)
        # Import only after DEER_FLOW_CONFIG_PATH is set.
        from agent_sandbox_provider.provider import AgentSandboxProvider

        print(f"[config] DeerFlow config: {config_path}")
        print(f"[config] namespace={os.getenv('AGENT_SANDBOX_NAMESPACE', 'default')} "
              f"warmpool={os.getenv('AGENT_SANDBOX_WARMPOOL', 'deer-flow-warmpool')}")
        provider = AgentSandboxProvider()
        try:
            sandbox_id = provider.acquire(thread_id="agent-sandbox-example", user_id="example-user")
            sandbox = provider.get(sandbox_id)
            if sandbox is None:
                raise RuntimeError(f"provider returned no sandbox for scope {sandbox_id}")
            try:
                exercise_sandbox(sandbox)
            finally:
                # release() destroys the SandboxClaim; the warm pool refills.
                provider.release(sandbox_id)
        finally:
            provider.shutdown()

    print("[cleanup] Sandbox claim destroyed")
    # The run's config lived in a TemporaryDirectory that is now gone, and
    # DEER_FLOW_CONFIG_PATH does not travel from this process to the user's
    # shell — so the follow-on command must select a persistent config copy
    # explicitly (and run inside the deer-flow uv environment, whose CLI a
    # fresh shell has no PATH entry for).
    example_dir = Path(__file__).resolve().parent
    print(f"\nDeerFlow config used for this run:\n\n{CONFIG_TEMPLATE}")
    print("Run a DeerFlow agent turn whose tools execute in the sandbox:")
    print(f"save the config above as {example_dir}/deer-flow-sandbox.yaml, then")
    print(f"  cd {example_dir}")
    print("  PYTHONPATH=$PWD DEER_FLOW_CONFIG_PATH=$PWD/deer-flow-sandbox.yaml \\")
    print("    uv run --project /path/to/deer-flow/backend \\")
    print("    deerflow --print \"Run python3 -c 'print(2 ** 16)' and report the output\"")


if __name__ == "__main__":
    main()
