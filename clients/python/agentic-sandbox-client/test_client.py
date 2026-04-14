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

import argparse
import time
import logging
import sys
import subprocess
from unittest.mock import MagicMock
from pydantic import ValidationError
from k8s_agent_sandbox import SandboxClient, SandboxTemplateNotFoundError
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxTracerConfig,
    ExecutionResult,
    FileEntry
)
from k8s_agent_sandbox.sandbox import Sandbox

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s', force=True)

def run_sandbox_tests(sandbox: Sandbox):
    """Tests methods on the Sandbox object (execution, files, etc)."""

    print("\n--- Testing Sandbox Status ---")
    status, message = sandbox.status()
    print(f"Status: {status}, Message: '{message}'")
    assert status == "SandboxReady", f"Expected 'SandboxReady', got '{status}'"
    print("--- Sandbox Status Test Passed! ---")

    print("\n--- Testing Command Execution ---")
    command_to_run = "echo 'Hello from the sandbox shruti!'"
    print(f"Executing command: '{command_to_run}'")

    result = sandbox.commands.run(command_to_run)

    print(f"Stdout: {result.stdout.strip()}")
    print(f"Stderr: {result.stderr.strip()}")
    print(f"Exit Code: {result.exit_code}")

    assert result.exit_code == 0
    assert result.stdout.strip() == "Hello from the sandbox shruti!"

    print("\n--- Command Execution Test Passed! ---")

    # Test introspection commands
    print("\n--- Testing Pod Introspection ---")

    print("\n--- Listing files in /app ---")
    list_files_result = sandbox.commands.run("ls -la /app")
    print(list_files_result.stdout)

    print("\n--- Printing environment variables ---")
    env_result = sandbox.commands.run("env")
    print(env_result.stdout)

    print("--- Introspection Tests Finished ---")

def test_file_operations(sandbox: Sandbox):
    """Tests file write, read, list, and existence checks."""
    print("\n--- Testing File Operations ---")
    file_content = "This is a test file."
    file_path = "test.txt"

    print(f"Writing content to '{file_path}'...")
    sandbox.files.write(file_path, file_content)

    print(f"Reading content from '{file_path}'...")
    read_content = sandbox.files.read(file_path).decode('utf-8')

    print(f"Read content: '{read_content}'")
    assert read_content == file_content

    print("--- File Operations Test Passed! ---")

    # Test list and exists
    print("\n--- Testing List and Exists ---")
    print(f"Checking if '{file_path}' exists...")
    exists = sandbox.files.exists(file_path)
    assert exists is True, f"Expected '{file_path}' to exist"

    print("Checking if 'non_existent_file.txt' exists...")
    not_exists = sandbox.files.exists("non_existent_file.txt")
    assert not_exists is False, "Expected 'non_existent_file.txt' to not exist"

    print("Listing files in '.' ...")
    files = sandbox.files.list(".")
    print(f"Files found: {[f.name for f in files]}")

    found = any(f.name == file_path for f in files)
    assert found is True, f"Expected '{file_path}' to be in the file list"

    file_entry = next(f for f in files if f.name == file_path)
    assert file_entry.size == len(file_content), f"Expected size {len(file_content)}, got {file_entry.size}"
    print("--- List and Exists Test Passed! ---")

    # Test introspection commands
    print("\n--- Testing Pod Introspection ---")

    print("\n--- Listing files in /app ---")
    list_files_result = sandbox.commands.run("ls -la /app")
    print(list_files_result.stdout)

    print("\n--- Printing environment variables ---")
    env_result = sandbox.commands.run("env")
    print(env_result.stdout)

    print("--- Introspection Tests Finished ---")

    print("\n--- Testing Pydantic Validation ---")

    # Test: ExecutionResult defaults (partial response)
    original_send_request = sandbox.connector.send_request

    mock_response = MagicMock()
    mock_response.json.return_value = {} # Empty response
    sandbox.connector.send_request = MagicMock(return_value=mock_response)

    print("Testing ExecutionResult defaults with empty response...")
    # This should not raise error because of defaults
    res = sandbox.commands.run("echo test")
    assert res.exit_code == -1
    assert res.stdout == ""
    assert isinstance(res, ExecutionResult)
    print("ExecutionResult defaults verified.")

    # Test: FileEntry validation (invalid type)
    mock_response.json.return_value = [{
        "name": "bad_file",
        "size": 100,
        "type": "invalid_type", # Invalid literal
        "mod_time": 12345.6
    }]

    print("Testing FileEntry validation with invalid type...")
    try:
        sandbox.files.list(".")
        raise AssertionError("RuntimeError not raised for invalid FileEntry type")
    except RuntimeError as e:
        print(f"Caught expected RuntimeError: {e}")
        assert "Server returned invalid file entry format" in str(e)

    # Restore original method
    sandbox.connector.send_request = original_send_request
    print("--- Pydantic Validation Tests Passed ---")


def test_claim_annotation(client: SandboxClient, template_name: str, namespace: str):
    print("\n--- Testing SandboxClaim Annotation ---")
    import uuid
    from datetime import datetime
    from k8s_agent_sandbox.constants import CLIENT_REQUEST_TIME_ANNOTATION

    claim_name = f"test-annotation-{uuid.uuid4().hex[:8]}"

    # Create claim using client
    client._create_claim(claim_name, template_name, namespace)

    try:
        # Get claim using k8s_helper
        claim = client.k8s_helper.custom_objects_api.get_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=namespace,
            plural="sandboxclaims",
            name=claim_name
        )

        annotations = claim.get("metadata", {}).get("annotations", {})
        print(f"Annotations: {annotations}")

        assert CLIENT_REQUEST_TIME_ANNOTATION in annotations, f"Expected annotation '{CLIENT_REQUEST_TIME_ANNOTATION}' missing"

        timestamp_str = annotations[CLIENT_REQUEST_TIME_ANNOTATION]
        print(f"Timestamp: {timestamp_str}")

        # Verify it can be parsed
        try:
            dt = datetime.fromisoformat(timestamp_str)
            assert dt.tzname() == 'UTC', "Timestamp should be in UTC"
            print(f"Parsed datetime: {dt}")
        except ValueError as e:
            raise AssertionError(f"Failed to parse timestamp '{timestamp_str}': {e}")

        print("--- SandboxClaim Annotation Test Passed! ---")

    finally:
        print(f"Cleaning up claim {claim_name}...")
        client._delete_claim(claim_name, namespace)

def run_sandbox_tests(sandbox: Sandbox):
    """Tests methods on the Sandbox object (execution, files, etc)."""

    print("\n--- Testing Sandbox Status ---")
    status, message = sandbox.status()
    print(f"Status: {status}, Message: '{message}'")
    assert status == "SandboxReady", f"Expected 'SandboxReady', got '{status}'"
    print("--- Sandbox Status Test Passed! ---")

    test_command_execution(sandbox)
    test_file_operations(sandbox)

def test_wrong_template_name(client: SandboxClient, namespace: str):
    print("\n--- Testing Wrong Template Name ---")
    wrong_template = "this-template-does-not-exist-123"
    print(f"Attempting to create sandbox with non-existent template '{wrong_template}'...")
    try:
        client.create_sandbox(wrong_template, namespace=namespace)
        raise AssertionError("Expected SandboxTemplateNotFoundError was not raised")
    except SandboxTemplateNotFoundError as e:
        print(f"Caught expected SandboxTemplateNotFoundError: {e}")
    print("--- Wrong Template Name Test Passed! ---")


def test_explicit_close_connection_and_persistence(client: SandboxClient, template_name: str, namespace: str):
    print("\n--- Testing Explicit Disconnect and Persistence ---")
    persist_sandbox = client.create_sandbox(template_name, namespace=namespace)
    persist_claim = persist_sandbox.claim_name

    print(f"Explicitly closing connection for sandbox '{persist_claim}'...")
    persist_sandbox.close_connection()
    assert not persist_sandbox.is_active, "Sandbox should be inactive after close_connection()"

    print("Checking active sandboxes list...")
    active_list = client.list_active_sandboxes()
    assert (namespace, persist_claim) not in active_list, "Sandbox with closed connection should be removed from active list"

    print(f"Re-attaching to sandbox '{persist_claim}' with closed connection...")
    reattached_sandbox = client.get_sandbox(persist_claim, namespace=namespace)
    assert reattached_sandbox.is_active, "Reattached sandbox should be active"
    assert (namespace, persist_claim) in client.list_active_sandboxes(), "Restored sandbox should be back in active list"
    assert persist_sandbox is not reattached_sandbox, "Expected different sandbox objects after close_connection and re-attach"
    assert persist_sandbox.connector.session is not reattached_sandbox.connector.session, "Expected different requests.Session objects after close_connection and re-attach"

    print("Cleaning up persisted sandbox...")
    reattached_sandbox.terminate()
    print("--- Explicit Close Connection Test Passed ---")

def test_creation_get_and_list_sandboxes(client: SandboxClient, template_name: str, namespace: str) -> tuple[Sandbox, Sandbox]:
    print(f"Creating sandbox with template '{template_name}' in namespace '{namespace}'...")
    sandbox = client.create_sandbox(template_name, namespace=namespace)
    print(f"Sandbox created with claim name: {sandbox.claim_name}")

    print(f"Creating second sandbox with template '{template_name}' in namespace '{namespace}'...")
    sandbox2 = client.create_sandbox(template_name, namespace=namespace)
    print(f"Sandbox 2 created with claim name: {sandbox2.claim_name}")

    print("\n--- Verifying Active Sandboxes ---")
    active_sandboxes = client.list_active_sandboxes()
    print(f"Active sandboxes: {active_sandboxes}")
    assert (sandbox.namespace, sandbox.claim_name) in active_sandboxes
    assert (sandbox2.namespace, sandbox2.claim_name) in active_sandboxes

    # Test get_sandbox
    print("\n--- Testing get_sandbox ---")
    reattached_sandbox = client.get_sandbox(sandbox.claim_name, namespace=namespace)
    print(f"Re-attached to sandbox: {reattached_sandbox.claim_name}")

    # Verify it is the same sandbox
    assert sandbox is reattached_sandbox, "Expected same sandbox objects"
    assert sandbox.connector.session is reattached_sandbox.connector.session, "Expected same requests.Session objects"

    reattached_result = reattached_sandbox.commands.run("echo 'Re-attached'")
    print(f"Re-attached execution result: {reattached_result.stdout.strip()}")
    assert reattached_result.exit_code == 0
    assert reattached_result.stdout.strip() == "Re-attached"
    print("\n--- get_sandbox Test Passed ---")

    return sandbox, sandbox2

def test_termination_and_deletion(client: SandboxClient, sandbox: Sandbox, sandbox2: Sandbox, namespace: str):
    print("\n--- Testing Termination and Get ---")
    print(f"Terminating sandbox {sandbox.claim_name}...")
    sandbox.terminate()

    client.delete_sandbox(sandbox.claim_name, namespace=namespace)

    print(f"Attempting to get terminated sandbox {sandbox.claim_name}...")
    # Wait for K8s to fully delete the resource
    start_time = time.monotonic()
    while True:
        try:
            client.get_sandbox(sandbox.claim_name, namespace=namespace)
            if time.monotonic() - start_time > 60:
                raise AssertionError(f"Sandbox {sandbox.claim_name} was not deleted within timeout")
            print("Sandbox still exists, waiting...")
            time.sleep(2)
        except RuntimeError as e:
            print(f"Caught expected RuntimeError: {e}")
            assert "not found" in str(e)
            break

    print("\n--- Verifying Sandbox Status after termination ---")
    status, message = sandbox.status()
    print(f"Status: {status}, Message: '{message}'")
    assert status == "SandboxNotFound", f"Expected 'SandboxNotFound', got '{status}'"
    print("--- Termination and Get Test Passed ---")

    print("\n--- Testing delete_all ---")
    # Ensure sandbox2 is still active
    assert (sandbox2.namespace, sandbox2.claim_name) in client.list_active_sandboxes()

    print("Calling client.delete_all()...")
    client.delete_all()

    # Verify client registry is empty
    active_sandboxes_after = client.list_active_sandboxes()
    assert len(active_sandboxes_after) == 0, f"Expected 0 active sandboxes, got {active_sandboxes_after}"

    # Verify sandbox2 state
    assert not sandbox2.is_active, "Sandbox 2 should be marked inactive"
    assert sandbox2.commands is None, "Sandbox 2 commands engine should be None"
    assert sandbox2.files is None, "Sandbox 2 files engine should be None"
    print("--- delete_all Test Passed ---")

    print("\n--- Verifying Sandbox 2 is unusable ---")
    try:
        sandbox2.commands.run("echo 'Should not work'")
        raise AssertionError("Sandbox 2 should be unusable after delete_all")
    except AttributeError:
        print("Verified: Sandbox 2 commands is None.")

    print("\n--- Verifying Sandbox 2 cannot be retrieved ---")
    # Wait for K8s to fully delete the resource
    start_time = time.monotonic()
    while True:
        try:
            client.get_sandbox(sandbox2.claim_name, namespace=namespace)
            if time.monotonic() - start_time > 60:
                raise AssertionError(f"Sandbox {sandbox2.claim_name} was not deleted within timeout")
            print("Sandbox still exists, waiting...")
            time.sleep(2)
        except RuntimeError as e:
            print(f"Caught expected RuntimeError: {e}")
            assert "not found" in str(e)
            break
    print("--- Sandbox 2 Retrieval Failure Verified ---")

def run_client_tests(client: SandboxClient, template_name: str, namespace: str):
    # Test Create, Get and List sandboxes
    sandbox, sandbox2 = test_creation_get_and_list_sandboxes(client, template_name, namespace)

    # Test wrong template name
    test_wrong_template_name(client, namespace)

    # Run Sandbox Tests
    run_sandbox_tests(sandbox)

    # Test persistence of Sandbox in Kubernetes cluster after client side disconnection
    test_explicit_close_connection_and_persistence(client, template_name, namespace)

    # Test Sandbox deletion at Kubernetes cluster
    test_termination_and_deletion(client, sandbox, sandbox2, namespace)


def test_client_cleanup_flag(client: SandboxClient, template_name: str, namespace: str, connection_config):
    print("\n--- Testing SandboxClient cleanup flag (Subprocess Simulation) ---")

    # Reconstruct the connection config dynamically for the subprocess
    if isinstance(connection_config, SandboxGatewayConnectionConfig):
        conn_code = f"SandboxGatewayConnectionConfig(gateway_name='{connection_config.gateway_name}', gateway_namespace='{connection_config.gateway_namespace}', server_port={connection_config.server_port})"
    elif isinstance(connection_config, SandboxDirectConnectionConfig):
        conn_code = f"SandboxDirectConnectionConfig(api_url='{connection_config.api_url}', server_port={connection_config.server_port})"
    else:
        conn_code = f"SandboxLocalTunnelConnectionConfig(server_port={connection_config.server_port})"

    script = f"""
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig, SandboxDirectConnectionConfig, SandboxLocalTunnelConnectionConfig
import sys

cleanup_flag = sys.argv[1] == 'True'
conn_config = {conn_code}
client = SandboxClient(connection_config=conn_config, cleanup=cleanup_flag)

sb = client.create_sandbox('{template_name}', namespace='{namespace}')
print(f"CLAIM_NAME:{{sb.claim_name}}")
"""

    print("Simulating script exit with cleanup=True...")
    res_true = subprocess.run([sys.executable, "-c", script, "True"], capture_output=True, text=True)
    if res_true.returncode != 0:
        raise RuntimeError(f"Subprocess failed:\nSTDOUT: {res_true.stdout}\nSTDERR: {res_true.stderr}")

    claim_true = next((line.split("CLAIM_NAME:")[1].strip() for line in res_true.stdout.splitlines() if line.startswith("CLAIM_NAME:")), None)
    if not claim_true:
        raise RuntimeError(f"Could not parse claim name.\nSTDOUT: {res_true.stdout}\nSTDERR: {res_true.stderr}")

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
        raise AssertionError(f"Sandbox {claim_true} should have been deleted by atexit!")
    print("Verified: Sandbox was successfully deleted on script exit.")

    print("Simulating script exit with cleanup=False...")
    res_false = subprocess.run([sys.executable, "-c", script, "False"], capture_output=True, text=True)
    if res_false.returncode != 0:
        raise RuntimeError(f"Subprocess failed:\nSTDOUT: {res_false.stdout}\nSTDERR: {res_false.stderr}")

    claim_false = next((line.split("CLAIM_NAME:")[1].strip() for line in res_false.stdout.splitlines() if line.startswith("CLAIM_NAME:")), None)
    if not claim_false:
        raise RuntimeError(f"Could not parse claim name.\nSTDOUT: {res_false.stdout}\nSTDERR: {res_false.stderr}")

    print(f"Created sandbox '{claim_false}' in subprocess. Verifying persistence...")

    # Verify the claim was NOT deleted by verifying we can cleanly reconnect to it
    sb_false = client.get_sandbox(claim_false, namespace=namespace)
    assert sb_false.is_active, f"Sandbox {claim_false} should still be active!"
    print("Verified: Sandbox persisted after script exit.")

    # Clean up the persisted sandbox explicitly
    sb_false.terminate()
    print("--- SandboxClient cleanup flag Test Passed ---")

def main(template_name: str, gateway_name: str | None, api_url: str | None, namespace: str,
               server_port: int, enable_tracing: bool):
    """
    Tests the Sandbox client by creating a sandbox, running a command,
    and then cleaning up.
    """

    print(
        f"--- Starting Sandbox Client Test (Namespace: {namespace}, Port: {server_port}) ---")
    if gateway_name:
        print(f"Mode: Gateway Discovery ({gateway_name})")
    elif api_url:
        print(f"Mode: Direct API URL ({api_url})")
    else:
        print("Mode: Local Port-Forward fallback")

    # Create Connection Config object
    if gateway_name:
        connection_config = SandboxGatewayConnectionConfig(
            gateway_name=gateway_name,
            server_port=server_port
        )
    elif api_url:
        connection_config = SandboxDirectConnectionConfig(
            api_url=api_url,
            server_port=server_port
        )
    else:
        connection_config = SandboxLocalTunnelConnectionConfig(
            server_port=server_port
        )

    tracer_config = SandboxTracerConfig(
        enable_tracing=enable_tracing,
        trace_service_name="sandbox-client-test"
    )

    client = SandboxClient(
        connection_config=connection_config,
        tracer_config=tracer_config,
        cleanup=True
    )

    test_client_cleanup_flag(client, template_name, namespace, connection_config)

    try:
        run_client_tests(client, template_name, namespace)

    except Exception as e:
        print(f"\n--- An error occurred during the test: {e} ---")
    finally:
        print("Cleaning up all sandboxes...")
        client.delete_all()
        print("\n--- Sandbox Client Test Finished ---")

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Test the Sandbox client.")
    parser.add_argument(
        "--template-name",
        default="python-sandbox-template",
        help="The name of the sandbox template to use for the test."
    )

    # Default is None to allow testing the Port-Forward fallback
    parser.add_argument(
        "--gateway-name",
        default=None,
        help="The name of the Gateway resource. If omitted, defaults to local port-forward mode."
    )

    parser.add_argument(
        "--api-url", help="Direct URL to router (e.g. http://localhost:8080)", default=None)
    parser.add_argument("--namespace", default="default",
                        help="Namespace to create sandbox in")
    parser.add_argument("--server-port", type=int, default=8888,
                        help="Port the sandbox container listens on")
    parser.add_argument("--enable-tracing",
                        action="store_true",
                        help="Enable OpenTelemetry tracing in the agentic-sandbox-client."
                        )

    args = parser.parse_args()

    main(
        template_name=args.template_name,
        gateway_name=args.gateway_name,
        api_url=args.api_url,
        namespace=args.namespace,
        server_port=args.server_port,
        enable_tracing=args.enable_tracing
    )
