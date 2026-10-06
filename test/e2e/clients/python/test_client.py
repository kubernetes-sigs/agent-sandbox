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
from unittest.mock import MagicMock

import pytest
from k8s_agent_sandbox import SandboxClient, SandboxWarmPoolNotFoundError
from k8s_agent_sandbox.models import (
    ExecutionResult,
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxTracerConfig,
)
from k8s_agent_sandbox.sandbox import Sandbox

pytestmark = pytest.mark.example


@pytest.fixture
def sandbox_connection_config(request, temp_namespace):
    if request.config.getoption("--gateway-name"):
        return SandboxGatewayConnectionConfig(
            gateway_name=request.config.getoption("--gateway-name"),
            server_port=request.config.getoption("--server-port"),
        )
    elif request.config.getoption("--api-url"):
        return SandboxDirectConnectionConfig(
            api_url=request.config.getoption("--api-url"),
            server_port=request.config.getoption("--server-port"),
        )
    else:
        return SandboxLocalTunnelConnectionConfig(
            server_port=request.config.getoption("--server-port"),
            router_namespace=temp_namespace,
        )


@pytest.fixture(scope="module")
def sandbox_tracer_config(request):
    return SandboxTracerConfig(
        enable_tracing=request.config.getoption("--enable-tracing"),
        trace_service_name=request.config.getoption("--trace-service-name"),
    )


@pytest.fixture
def sandbox_client(sandbox_connection_config, sandbox_tracer_config):
    client = SandboxClient(
        connection_config=sandbox_connection_config,
        tracer_config=sandbox_tracer_config,
        cleanup=True,
    )
    yield client
    client.delete_all()


@pytest.fixture
def sandbox(sandbox_warmpool, temp_namespace, sandbox_client):
    sandbox = sandbox_client.create_sandbox(sandbox_warmpool, namespace=temp_namespace)
    return sandbox


@pytest.fixture
def sandbox2(sandbox_warmpool, temp_namespace, sandbox_client):
    sandbox = sandbox_client.create_sandbox(sandbox_warmpool, namespace=temp_namespace)
    return sandbox


@pytest.fixture
def sandbox_create_script(
    sandbox_connection_config,
    sandbox_warmpool,
    temp_namespace,
):
    if isinstance(sandbox_connection_config, SandboxGatewayConnectionConfig):
        conn_code = f"SandboxGatewayConnectionConfig(gateway_name='{sandbox_connection_config.gateway_name}', gateway_namespace='{sandbox_connection_config.gateway_namespace}', server_port={sandbox_connection_config.server_port})"
    elif isinstance(sandbox_connection_config, SandboxDirectConnectionConfig):
        conn_code = f"SandboxDirectConnectionConfig(api_url='{sandbox_connection_config.api_url}', server_port={sandbox_connection_config.server_port})"
    else:
        conn_code = f"SandboxLocalTunnelConnectionConfig(server_port={sandbox_connection_config.server_port}, router_namespace='{sandbox_connection_config.router_namespace}')"

    script = f"""
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig, SandboxDirectConnectionConfig, SandboxLocalTunnelConnectionConfig
import sys

cleanup_flag = sys.argv[1] == 'True'
conn_config = {conn_code}
client = SandboxClient(connection_config=conn_config, cleanup=cleanup_flag)

sb = client.create_sandbox('{sandbox_warmpool}', namespace='{temp_namespace}')
print(f"CLAIM_NAME:{{sb.claim_name}}")
"""
    return script


def test_sandbox_cleanup_flag_true(
    sandbox_create_script,
    sandbox_client,
    sandbox_warmpool,
    temp_namespace,
):
    print("Simulating script exit with cleanup=True...")
    res_true = subprocess.run(
        [sys.executable, "-c", sandbox_create_script, "True"],
        capture_output=True,
        text=True,
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

    # Verify the claim was successfully deleted by the OS closing the subprocess
    start_time = time.monotonic()
    deleted = False
    while time.monotonic() - start_time < 60:
        try:
            sandbox_client.get_sandbox(claim_true, namespace=temp_namespace)
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


def test_sandbox_cleanup_flag_false(
    sandbox_create_script, sandbox_client, sandbox_warmpool, temp_namespace
):
    print("Simulating script exit with cleanup=False...")
    res_false = subprocess.run(
        [sys.executable, "-c", sandbox_create_script, "False"],
        capture_output=True,
        text=True,
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
    sb_false = sandbox_client.get_sandbox(claim_false, temp_namespace)
    assert sb_false.is_active, f"Sandbox {claim_false} should still be active!"
    print("Verified: Sandbox persisted after script exit.")

    # Clean up the persisted sandbox explicitly
    sb_false.terminate()
    print("--- SandboxClient cleanup flag Test Passed ---")


def test_command_execution(sandbox: Sandbox, sandbox_warmpool, deploy_router):
    """Tests command execution and pod introspection."""
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


def test_file_operations(sandbox: Sandbox, deploy_router):
    """Tests file write, read, list, and existence checks."""
    print("\n--- Testing File Operations ---")
    file_content = "This is a test file."
    file_path = "test.txt"

    print(f"Writing content to '{file_path}'...")
    sandbox.files.write(file_path, file_content)

    print(f"Reading content from '{file_path}'...")
    read_content = sandbox.files.read(file_path).decode("utf-8")

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
    assert file_entry.size == len(file_content), (
        f"Expected size {len(file_content)}, got {file_entry.size}"
    )
    print("--- List and Exists Test Passed! ---")

    print("\n--- Testing Pydantic Validation ---")

    # Test: ExecutionResult defaults (partial response)
    original_send_request = sandbox.connector.send_request

    mock_response = MagicMock()
    mock_response.json.return_value = {}  # Empty response
    sandbox.connector.send_request = MagicMock(return_value=mock_response)

    print("Testing ExecutionResult defaults with empty response...")
    # This should not raise error because of defaults
    res = sandbox.commands.run("echo test")
    assert res.exit_code == -1
    assert res.stdout == ""
    assert isinstance(res, ExecutionResult)
    print("ExecutionResult defaults verified.")

    # Test: FileEntry validation (invalid type)
    mock_response.json.return_value = [
        {
            "name": "bad_file",
            "size": 100,
            "type": "invalid_type",  # Invalid literal
            "mod_time": 12345.6,
        }
    ]

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


def test_termination_and_deletion(
    sandbox_client: SandboxClient,
    sandbox: Sandbox,
    sandbox2: Sandbox,
    temp_namespace: str,
    sandbox_warmpool,
):
    print("\n--- Testing Termination and Get ---")
    print(f"Terminating sandbox {sandbox.claim_name}...")
    sandbox.terminate()

    print(f"Attempting to get terminated sandbox {sandbox.claim_name}...")
    # Wait for K8s to fully delete the resource
    start_time = time.monotonic()
    while True:
        try:
            sandbox_client.get_sandbox(sandbox.claim_name, namespace=temp_namespace)
            if time.monotonic() - start_time > 60:
                raise AssertionError(
                    f"Sandbox {sandbox.claim_name} was not deleted within timeout"
                )
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
    assert (
        sandbox2.namespace,
        sandbox2.claim_name,
    ) in sandbox_client.list_active_sandboxes()

    print("Calling client.delete_all()...")
    sandbox_client.delete_all()

    # Verify client registry is empty
    active_sandboxes_after = sandbox_client.list_active_sandboxes()
    assert len(active_sandboxes_after) == 0, (
        f"Expected 0 active sandboxes, got {active_sandboxes_after}"
    )

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
            sandbox_client.get_sandbox(sandbox2.claim_name, namespace=temp_namespace)
            if time.monotonic() - start_time > 60:
                raise AssertionError(
                    f"Sandbox {sandbox2.claim_name} was not deleted within timeout"
                )
            print("Sandbox still exists, waiting...")
            time.sleep(2)
        except RuntimeError as e:
            print(f"Caught expected RuntimeError: {e}")
            assert "not found" in str(e)
            break
    print("--- Sandbox 2 Retrieval Failure Verified ---")


def test_wrong_warmpool_name(sandbox_client: SandboxClient, temp_namespace):
    print("\n--- Testing Wrong Warmpool Name ---")
    wrong_warmpool = "this-warmpool-does-not-exist-123"
    print(
        f"Attempting to create sandbox with non-existent warm pool '{wrong_warmpool}'..."
    )
    try:
        sandbox_client.create_sandbox(wrong_warmpool, namespace=temp_namespace)
        raise AssertionError("Expected SandboxWarmPoolNotFoundError was not raised")
    except SandboxWarmPoolNotFoundError as e:
        print(f"Caught expected SandboxWarmPoolNotFoundError: {e}")
    print("--- Wrong Warmpool Name Test Passed! ---")


def test_explicit_close_connection_and_persistence(
    sandbox_client: SandboxClient, sandbox_warmpool, temp_namespace
):
    print("\n--- Testing Explicit Disconnect and Persistence ---")
    persist_sandbox = sandbox_client.create_sandbox(
        sandbox_warmpool, namespace=temp_namespace
    )
    persist_claim = persist_sandbox.claim_name

    print(f"Explicitly closing connection for sandbox '{persist_claim}'...")
    persist_sandbox.close_connection()
    assert not persist_sandbox.is_active, (
        "Sandbox should be inactive after close_connection()"
    )

    print("Checking active sandboxes list...")
    active_list = sandbox_client.list_active_sandboxes()
    assert (temp_namespace, persist_claim) not in active_list, (
        "Sandbox with closed connection should be removed from active list"
    )

    print(f"Re-attaching to sandbox '{persist_claim}' with closed connection...")
    reattached_sandbox = sandbox_client.get_sandbox(
        persist_claim, namespace=temp_namespace
    )
    assert reattached_sandbox.is_active, "Reattached sandbox should be active"
    assert (temp_namespace, persist_claim) in sandbox_client.list_active_sandboxes(), (
        "Restored sandbox should be back in active list"
    )
    assert persist_sandbox is not reattached_sandbox, (
        "Expected different sandbox objects after close_connection and re-attach"
    )
    assert (
        persist_sandbox.connector.session is not reattached_sandbox.connector.session
    ), (
        "Expected different requests.Session objects after close_connection and re-attach"
    )

    print("Cleaning up persisted sandbox...")
    reattached_sandbox.terminate()
    print("--- Explicit Close Connection Test Passed ---")


def test_creation_get_and_list_sandboxes(
    sandbox_client: SandboxClient,
    sandbox_warmpool,
    temp_namespace,
    deploy_router,
):
    print(
        f"Creating sandbox with warm pool '{sandbox_warmpool}' in namespace '{temp_namespace}'..."
    )
    sandbox = sandbox_client.create_sandbox(sandbox_warmpool, namespace=temp_namespace)
    print(f"Sandbox created with claim name: {sandbox.claim_name}")

    print(
        f"Creating second sandbox with warm pool '{sandbox_warmpool}' in namespace '{temp_namespace}'..."
    )
    sandbox2 = sandbox_client.create_sandbox(sandbox_warmpool, namespace=temp_namespace)
    print(f"Sandbox 2 created with claim name: {sandbox2.claim_name}")

    print("\n--- Verifying Active Sandboxes ---")
    active_sandboxes = sandbox_client.list_active_sandboxes()
    print(f"Active sandboxes: {active_sandboxes}")
    assert (sandbox.namespace, sandbox.claim_name) in active_sandboxes
    assert (sandbox2.namespace, sandbox2.claim_name) in active_sandboxes

    # Test get_sandbox
    print("\n--- Testing get_sandbox ---")
    reattached_sandbox = sandbox_client.get_sandbox(
        sandbox.claim_name, namespace=temp_namespace
    )
    print(f"Re-attached to sandbox: {reattached_sandbox.claim_name}")

    # Verify it is the same sandbox
    assert sandbox is reattached_sandbox, "Expected same sandbox objects"
    assert sandbox.connector.session is reattached_sandbox.connector.session, (
        "Expected same requests.Session objects"
    )

    reattached_result = reattached_sandbox.commands.run("echo 'Re-attached'")
    print(f"Re-attached execution result: {reattached_result.stdout.strip()}")
    assert reattached_result.exit_code == 0
    assert reattached_result.stdout.strip() == "Re-attached"
    print("\n--- get_sandbox Test Passed ---")


def test_claim_annotation(
    sandbox_client: SandboxClient, sandbox_warmpool, temp_namespace
):
    print("\n--- Testing SandboxClaim Annotation ---")
    import uuid
    from datetime import datetime

    from k8s_agent_sandbox.constants import (
        CLAIM_API_GROUP,
        CLAIM_API_VERSION,
        CLAIM_PLURAL_NAME,
        CLIENT_REQUEST_TIME_ANNOTATION,
    )

    claim_name = f"test-annotation-{uuid.uuid4().hex[:8]}"
    # Create claim using client
    sandbox_client._create_claim(claim_name, sandbox_warmpool, temp_namespace)

    try:
        # Get claim using k8s_helper
        claim = (
            sandbox_client.k8s_helper.custom_objects_api.get_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=temp_namespace,
                plural=CLAIM_PLURAL_NAME,
                name=claim_name,
            )
        )

        annotations = claim.get("metadata", {}).get("annotations", {})
        print(f"Annotations: {annotations}")

        assert CLIENT_REQUEST_TIME_ANNOTATION in annotations, (
            f"Expected annotation '{CLIENT_REQUEST_TIME_ANNOTATION}' missing"
        )

        timestamp_str = annotations[CLIENT_REQUEST_TIME_ANNOTATION]
        print(f"Timestamp: {timestamp_str}")

        # Verify it can be parsed
        try:
            dt = datetime.fromisoformat(timestamp_str)
            assert dt.tzname() == "UTC", "Timestamp should be in UTC"
            print(f"Parsed datetime: {dt}")
        except ValueError as e:
            raise AssertionError(
                f"Failed to parse timestamp '{timestamp_str}': {e}"
            ) from e

        print("--- SandboxClaim Annotation Test Passed! ---")

    finally:
        print(f"Cleaning up claim {claim_name}...")
        sandbox_client._delete_claim(claim_name, temp_namespace)


def test_volume_claim_templates(
    sandbox_client: SandboxClient,
    sandbox_warmpool,
    temp_namespace,
    deploy_router,
):
    print("\n--- Testing Custom Volume Claim Templates on SandboxClaim ---")

    storage_class = os.getenv("SANDBOX_TEST_STORAGE_CLASS")
    custom_vcts = [
        {
            "metadata": {"name": "custom-workspace"},
            "spec": {
                "accessModes": ["ReadWriteOnce"],
                "resources": {"requests": {"storage": "4Gi"}},
            },
        }
    ]
    if storage_class:
        custom_vcts[0]["spec"]["storageClassName"] = storage_class

    print("Creating sandbox with custom volume claim templates...")
    sandbox = sandbox_client.create_sandbox(
        sandbox_warmpool, namespace=temp_namespace, volume_claim_templates=custom_vcts
    )
    print(f"Sandbox created with claim name: {sandbox.claim_name}")

    try:
        # Verify that volumeClaimTemplates was propagated to the SandboxClaim spec
        print("Verifying SandboxClaim spec.volumeClaimTemplates...")
        claim_res = sandbox_client.k8s_helper.get_sandbox_claim(
            sandbox.claim_name, temp_namespace
        )
        assert claim_res is not None, f"SandboxClaim {sandbox.claim_name} should exist"
        claim_spec = claim_res.get("spec", {})
        claim_vcts = claim_spec.get("volumeClaimTemplates", [])
        assert len(claim_vcts) == 1, (
            f"Expected 1 volumeClaimTemplate on SandboxClaim, got {len(claim_vcts)}"
        )
        assert claim_vcts[0].get("metadata", {}).get("name") == "custom-workspace", (
            "Volume claim template name mismatch in SandboxClaim"
        )

        # Verify that volumeClaimTemplates was propagated to the Sandbox spec
        print("Verifying Sandbox spec.volumeClaimTemplates...")
        sandbox_res = sandbox_client.k8s_helper.get_sandbox(
            sandbox.sandbox_id, temp_namespace
        )
        assert sandbox_res is not None, f"Sandbox {sandbox.sandbox_id} should exist"
        sandbox_spec = sandbox_res.get("spec", {})
        sandbox_vcts = sandbox_spec.get("volumeClaimTemplates", [])
        assert len(sandbox_vcts) == 1, (
            f"Expected 1 volumeClaimTemplates in Sandbox, got {len(sandbox_vcts)}"
        )

        vct = sandbox_vcts[0]
        assert vct.get("metadata", {}).get("name") == "custom-workspace", (
            "Volume claim template name mismatch in Sandbox"
        )
        assert vct.get("spec", {}).get("accessModes") == ["ReadWriteOnce"], (
            "Volume claim template accessModes mismatch in Sandbox"
        )
        if storage_class:
            assert vct.get("spec", {}).get("storageClassName") == storage_class, (
                "Volume claim template storageClassName mismatch in Sandbox"
            )
        else:
            assert (
                vct.get("spec", {}).get("storageClassName") is None
                or vct.get("spec", {}).get("storageClassName") == ""
            ), "Volume claim template storageClassName mismatch in Sandbox"
        assert (
            vct.get("spec", {}).get("resources", {}).get("requests", {}).get("storage")
            == "4Gi"
        ), "Volume claim template storage request mismatch in Sandbox"

        # Verify that the PersistentVolumeClaim resource was created with the expected properties
        print("Verifying PVC creation in cluster...")
        pvc_name = f"custom-workspace-{sandbox.sandbox_id}"
        pvc_res = sandbox_client.k8s_helper.core_v1_api.read_namespaced_persistent_volume_claim(
            pvc_name, temp_namespace
        )
        assert pvc_res is not None, f"PVC {pvc_name} should exist"
        assert pvc_res.spec.resources.requests is not None
        assert pvc_res.spec.resources.requests.get("storage") == "4Gi", (
            "PVC storage request mismatch in cluster"
        )
        if storage_class:
            assert pvc_res.spec.storage_class_name == storage_class, (
                "PVC storageClassName mismatch in cluster"
            )
        else:
            assert pvc_res.spec.storage_class_name, (
                "PVC storage_class_name should be set by the cluster default"
            )

        print("Running command to verify sandbox is operational...")
        res = sandbox.commands.run("df -h")
        print(f"Disk space output:\n{res.stdout}")
        assert res.exit_code == 0, (
            f"Command df -h failed with exit code {res.exit_code}: {res.stderr}"
        )
    finally:
        print("Cleaning up sandbox...")
        sandbox.terminate()
    print("--- Volume Claim Templates Test Passed! ---")
