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

"""
Integration test for PodSnapshotSandboxClient.
"""

import argparse
import time
import logging
from kubernetes import config
from k8s_agent_sandbox.gke_extensions.snapshots.podsnapshot_client import (
    PodSnapshotSandboxClient,
)
from k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine import SnapshotResponse
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)


WAIT_TIME_SECONDS = 10

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s", force=True
)


def test_snapshot_response(snapshot_response: SnapshotResponse, snapshot_name: str):
    assert hasattr(
        snapshot_response, "trigger_name"
    ), "snapshot response missing 'trigger_name' attribute"

    print(f"Trigger Name: {snapshot_response.trigger_name}")

    assert snapshot_response.trigger_name.startswith(
        snapshot_name
    ), f"Expected trigger name prefix '{snapshot_name}', but got '{snapshot_response.trigger_name}'"
    assert (
        snapshot_response.success
    ), f"Expected success=True, but got False. Reason: {snapshot_response.error_reason}"
    assert snapshot_response.error_code == 0


def wait_for_snapshot_ready(sandbox, snapshot_uid: str, max_retries: int = 30, sleep_time: int = 2) -> bool:
    """Helper to poll until a specific snapshot UID is reported as ready."""
    for _ in range(max_retries):
        check_list = sandbox.snapshots.list(filter_by={"grouping_labels": {"tenant-id": "test-tenant", "user-id": "test-user"}})
        if check_list.success and any(s.snapshot_uid == snapshot_uid for s in check_list.snapshots):
            print(f"Snapshot '{snapshot_uid}' is ready.")
            return True
        time.sleep(sleep_time)
    print(f"Warning: Snapshot '{snapshot_uid}' did not become ready in time.")
    return False


def test_manual_snapshots(client, sandbox, template_name: str, namespace: str) -> tuple[str, str]:
    """Tests creating manual snapshots and restoring a sandbox from the latest snapshot."""
    first_snapshot_trigger_name = "test-snapshot-10"
    second_snapshot_trigger_name = "test-snapshot-20"

    time.sleep(WAIT_TIME_SECONDS)
    print(
        f"Creating first manual pod snapshot '{first_snapshot_trigger_name}' after {WAIT_TIME_SECONDS} seconds..."
    )
    snapshot_response = sandbox.snapshots.create(first_snapshot_trigger_name)
    test_snapshot_response(snapshot_response, first_snapshot_trigger_name)
    first_snapshot_uid = snapshot_response.snapshot_uid
    print(f"First snapshot UID: {first_snapshot_uid}")

    time.sleep(WAIT_TIME_SECONDS)

    print(
        f"\nCreating second manual pod snapshot '{second_snapshot_trigger_name}' after {WAIT_TIME_SECONDS} seconds..."
    )
    snapshot_response = sandbox.snapshots.create(second_snapshot_trigger_name)
    test_snapshot_response(snapshot_response, second_snapshot_trigger_name)
    second_snapshot_uid = snapshot_response.snapshot_uid
    print(f"Recent snapshot UID: {second_snapshot_uid}")

    # Wait longer for the PodSnapshot controller's cache to recognize the new snapshot as the latest
    print(f"Waiting for second snapshot '{second_snapshot_uid}' to become ready...")
    wait_for_snapshot_ready(sandbox, second_snapshot_uid)

    print(
        f"\nChecking if sandbox was restored from latest snapshot '{second_snapshot_uid}'..."
    )

    return first_snapshot_uid, second_snapshot_uid


def test_suspend_resume(sandbox) -> str:
    """Tests suspending and resuming a sandbox and verifying it restored correctly."""
    print("\n======= Testing Suspend and Resume =======")

    print(f"\nSuspending sandbox '{sandbox.sandbox_id}'...")
    suspend_result = sandbox.suspend(snapshot_before_suspend=True)
    assert suspend_result.success, f"Suspend failed: {suspend_result.error_reason}"
    assert sandbox.is_suspended(), "Sandbox should be suspended."
    suspend_third_snapshot_uid = suspend_result.snapshot_response.snapshot_uid if suspend_result.snapshot_response else 'None'
    print(f"Sandbox suspended. Snapshot UID: {suspend_third_snapshot_uid}")

    print(f"Waiting for suspend snapshot '{suspend_third_snapshot_uid}' to become ready...")
    wait_for_snapshot_ready(sandbox, suspend_third_snapshot_uid)

    print(f"\nResuming sandbox '{sandbox.sandbox_id}'...")
    resume_result = sandbox.resume()
    assert resume_result.success, f"Resume failed: {resume_result.error_reason}"
    assert resume_result.restored_from_snapshot, "Sandbox should have been restored from snapshot."
    assert not sandbox.is_suspended(), "Sandbox should not be suspended after resume."
    print(f"Sandbox resumed. Restored from Snapshot UID: {resume_result.snapshot_uid}")
    
    return suspend_third_snapshot_uid


def test_list_and_delete(sandbox, first_snapshot_uid: str, second_snapshot_uid: str, suspend_third_snapshot_uid: str):
    """Tests listing all snapshots and verifying snapshot deletion."""
    print("\n======= Testing List and Delete =======")
    print(f"\nListing all snapshots for sandbox '{sandbox.sandbox_id}'...")
    list_result = sandbox.snapshots.list(filter_by={"grouping_labels": {"tenant-id": "test-tenant", "user-id": "test-user"}})
    assert list_result.success, list_result.error_reason

    for snap in list_result.snapshots:
        print(
            f"Snapshot UID: {snap.snapshot_uid}, Source Pod: {snap.source_pod}, Creation Time: {snap.creation_timestamp}"
        )
    assert (
        len(list_result.snapshots) == 3
    ), f"Expected 3 snapshots, but got {len(list_result.snapshots)}"
    assert (
        list_result.snapshots[0].snapshot_uid == suspend_third_snapshot_uid
    ), f"Expected most recent snapshot UID '{suspend_third_snapshot_uid}', but got '{list_result.snapshots[0].snapshot_uid}'"
    assert (
        list_result.snapshots[2].snapshot_uid == first_snapshot_uid
    ), f"Expected older snapshot UID '{first_snapshot_uid}', but got '{list_result.snapshots[2].snapshot_uid}'"

    print(
        f"\nDeleting snapshot '{suspend_third_snapshot_uid}' of the sandbox '{sandbox.sandbox_id}'..."
    )
    delete_result = sandbox.snapshots.delete(snapshot_uid=suspend_third_snapshot_uid)
    assert delete_result.success, delete_result.error_reason
    assert (
        len(delete_result.deleted_snapshots) == 1
    ), f"Expected 1 deleted snapshot, but got {len(delete_result.deleted_snapshots)}"
    assert (
        delete_result.deleted_snapshots[0] == suspend_third_snapshot_uid
    ), f"Expected deleted snapshot UID '{suspend_third_snapshot_uid}', but got '{delete_result.deleted_snapshots[0]}''"
    print(f"Snapshot '{suspend_third_snapshot_uid}' deleted successfully.")

    print(f"\nDeleting all snapshots for sandbox '{sandbox.sandbox_id}'...")
    delete_result = sandbox.snapshots.delete_all()
    assert delete_result.success, delete_result.error_reason
    assert (
        len(delete_result.deleted_snapshots) == 2
    ), f"Expected 2 deleted snapshots, but got {len(delete_result.deleted_snapshots)}"
    assert (
        first_snapshot_uid in delete_result.deleted_snapshots and second_snapshot_uid in delete_result.deleted_snapshots
    ), f"Expected deleted snapshot UIDs to include '{first_snapshot_uid}' and '{second_snapshot_uid}'"
    print(f"Snapshots deleted successfully.")


def main(
    template_name: str,
    api_url: str | None,
    namespace: str,
    server_port: int,
):
    """
    Tests the Sandbox client by creating a sandbox, running a command,
    and then cleaning up.
    """

    print(
        f"--- Starting Sandbox Client Test (Namespace: {namespace}, Port: {server_port}) ---"
    )

    # Load kube config
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

    client = None
    try:
        print("\n***** Phase 1: Starting Counter *****")

        if api_url:
            connection_config = SandboxDirectConnectionConfig(
                api_url=api_url, server_port=server_port
            )
        else:
            connection_config = SandboxLocalTunnelConnectionConfig(
                server_port=server_port
            )

        client = PodSnapshotSandboxClient(connection_config=connection_config)

        print("\n======= Testing Pod Snapshot Extension =======")

        sandbox = client.create_sandbox(template_name, namespace=namespace)
        
        first_snapshot_uid, second_snapshot_uid = test_manual_snapshots(
            client, sandbox, template_name, namespace
        )
        suspend_third_snapshot_uid = test_suspend_resume(sandbox)
        
        time.sleep(WAIT_TIME_SECONDS)
        
        test_list_and_delete(
            sandbox, first_snapshot_uid, second_snapshot_uid, suspend_third_snapshot_uid
        )

        print("--- Pod Snapshot Test Passed! ---")

    except Exception as e:
        print(f"\n--- An error occurred during the test: {e} ---")
    finally:
        print("Cleaning up all sandboxes...")
        if client:
            client.delete_all()
        print("\n--- Sandbox Client Test Finished ---")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Test the Sandbox client.")
    parser.add_argument(
        "--template-name",
        default="python-sandbox-template",
        help="The name of the sandbox template to use for the test.",
    )

    parser.add_argument(
        "--api-url",
        help="Direct URL to router (e.g. http://localhost:8080)",
        default=None,
    )
    parser.add_argument(
        "--namespace", default="default", help="Namespace to create sandbox in"
    )
    parser.add_argument(
        "--server-port",
        type=int,
        default=8888,
        help="Port the sandbox container listens on",
    )

    args = parser.parse_args()

    main(
        template_name=args.template_name,
        api_url=args.api_url,
        namespace=args.namespace,
        server_port=args.server_port,
    )
