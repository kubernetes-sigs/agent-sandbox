# Copyright 2025 The Kubernetes Authors.
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

import os
import subprocess
import sys
import time
import tempfile
from test.e2e.clients.python.framework.context import TestContext

import pytest
import yaml
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)

TEST_MANIFESTS_DIR = "test/e2e/clients/python/test_manifests"
TEMPLATE_YAML_PATH = os.path.join(TEST_MANIFESTS_DIR, "sandbox_template.yaml")
WARMPOOL_YAML_PATH = os.path.join(TEST_MANIFESTS_DIR, "sandbox_warmpool.yaml")

ROUTER_YAML_PATH = (
    "clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml"
)
GATEWAY_YAML_PATH = (
    "clients/python/agentic-sandbox-client/gateway-kind/gateway-kind.yaml"
)
GATEWAY_NAME = "kind-gateway"


def get_image_tag(env_var="IMAGE_TAG", default="latest"):
    """Retrieves the image tag from environment variable or returns default"""
    return os.environ.get(env_var, default)


def get_image_prefix(env_var="IMAGE_PREFIX", default="kind.local/"):
    """Retrieves the image prefix from environment variable or returns default"""
    return os.environ.get(env_var, default)


def wait_until_sandbox_routable(sandbox):
    """Probe the data path with GET before the first POST /execute.

    Claim Ready does not mean the router can dial the runtime yet. GET is
    still retried on 5xx; POST /execute is not.
    """
    sandbox.files.exists(".")


def run_sdk_tests(sandbox):
    """Runs basic SDK operations to validate functionality"""
    wait_until_sandbox_routable(sandbox)
    # Test execution
    result = sandbox.commands.run("echo 'Hello from SDK'")
    print(f"Run result: {result}")
    assert result.stdout == "Hello from SDK\n", f"Unexpected stdout: {result.stdout}"
    assert result.stderr == "", f"Unexpected stderr: {result.stderr}"
    assert result.exit_code == 0, f"Unexpected exit code: {result.exit_code}"

    # Test File Write / Read
    file_content = "This is a test file."
    file_path = "test.txt"  # Relative path inside the sandbox
    print(f"Writing content to '{file_path}'...")
    sandbox.files.write(file_path, file_content)

    print(f"Reading content from '{file_path}'...")
    read_content = sandbox.files.read(file_path).decode("utf-8")
    print(f"Read content: '{read_content}'")
    assert read_content == file_content, f"File content mismatch: {read_content}"

    with tempfile.TemporaryFile() as destination:
        written = sandbox.files.read_to(file_path, destination)
        destination.seek(0)
        streamed_content = destination.read().decode("utf-8")
    assert written == len(file_content.encode("utf-8"))
    assert streamed_content == file_content


def validate_client_cleanup(
    client: SandboxClient, warmpool_name: str, namespace: str, connection_config
):
    print("\n--- Testing SandboxClient cleanup flag (Subprocess Simulation) ---")

    # Reconstruct the connection config dynamically for the subprocess
    if isinstance(connection_config, SandboxGatewayConnectionConfig):
        conn_code = f"SandboxGatewayConnectionConfig(gateway_name='{connection_config.gateway_name}', gateway_namespace='{connection_config.gateway_namespace}', server_port={connection_config.server_port})"
    elif isinstance(connection_config, SandboxDirectConnectionConfig):
        conn_code = f"SandboxDirectConnectionConfig(api_url='{connection_config.api_url}', server_port={connection_config.server_port})"
    else:
        conn_code = f"SandboxLocalTunnelConnectionConfig(server_port={connection_config.server_port}, router_namespace='{connection_config.router_namespace}')"

    script = f"""
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig, SandboxDirectConnectionConfig, SandboxLocalTunnelConnectionConfig
import sys

cleanup_flag = sys.argv[1] == 'True'
conn_config = {conn_code}
client = SandboxClient(connection_config=conn_config, cleanup=cleanup_flag)

sb = client.create_sandbox('{warmpool_name}', namespace='{namespace}')
print(f"CLAIM_NAME:{{sb.claim_name}}")
"""

    print("Simulating script exit with cleanup=True...")
    res_true = subprocess.run(
        [sys.executable, "-c", script, "True"], capture_output=True, text=True
    )
    if res_true.returncode != 0:
        raise RuntimeError(
            f"Subprocess failed:\nSTDOUT: {res_true.stdout}\nSTDERR: {res_true.stderr}"
        )

    claim_true = next(
        (
            line.split("CLAIM_NAME:")[1].strip()
            for line in res_true.stdout.splitlines()
            if line.startswith("CLAIM_NAME:")
        ),
        None,
    )
    if not claim_true:
        raise RuntimeError(
            f"Could not parse claim name.\nSTDOUT: {res_true.stdout}\nSTDERR: {res_true.stderr}"
        )

    print(f"Created sandbox '{claim_true}' in subprocess. Verifying deletion...")

    # Verify the claim was successfully deleted by the OS closing the subprocess
    start_time = time.monotonic()
    deleted = False
    while time.monotonic() - start_time < 60:
        try:
            client.get_sandbox(claim_true, namespace=namespace)
            time.sleep(2)
        except Exception as e:
            if "not found" in str(e).lower():
                deleted = True
                break
            time.sleep(2)

    if not deleted:
        raise AssertionError(
            f"Sandbox {claim_true} should have been deleted by atexit!"
        )
    print("Verified: Sandbox was successfully deleted on script exit.")

    print("Simulating script exit with cleanup=False...")
    res_false = subprocess.run(
        [sys.executable, "-c", script, "False"], capture_output=True, text=True
    )
    if res_false.returncode != 0:
        raise RuntimeError(
            f"Subprocess failed:\nSTDOUT: {res_false.stdout}\nSTDERR: {res_false.stderr}"
        )

    claim_false = next(
        (
            line.split("CLAIM_NAME:")[1].strip()
            for line in res_false.stdout.splitlines()
            if line.startswith("CLAIM_NAME:")
        ),
        None,
    )
    if not claim_false:
        raise RuntimeError(
            f"Could not parse claim name.\nSTDOUT: {res_false.stdout}\nSTDERR: {res_false.stderr}"
        )

    print(f"Created sandbox '{claim_false}' in subprocess. Verifying persistence...")

    # Verify the claim was NOT deleted by verifying we can cleanly reconnect to it
    sb_false = client.get_sandbox(claim_false, namespace=namespace)
    assert sb_false.is_active, f"Sandbox {claim_false} should still be active!"
    print("Verified: Sandbox persisted after script exit.")

    # Clean up the persisted sandbox explicitly
    sb_false.terminate()
    print("--- SandboxClient cleanup flag Test Passed ---")
