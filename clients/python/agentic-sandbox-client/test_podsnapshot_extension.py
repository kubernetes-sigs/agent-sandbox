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
from k8s_agent_sandbox.gke_extensions.snapshots.podsnapshot_client import PodSnapshotSandboxClient
from k8s_agent_sandbox.gke_extensions.snapshots.snapshot_engine import SnapshotResponse
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)


WAIT_TIME_SECONDS = 10

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s', force=True)
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

    first_snapshot_trigger_name = "test-snapshot-10"
    second_snapshot_trigger_name = "test-snapshot-20"

    # grouping labels used in PodSnapshotPolicy to group snapshots - tenant-id and user-id
    grouping_labels = {"tenant-id": "test-tenant", "user-id": "test-user"}

    client = None
    try:
        print("\n***** Phase 1: Starting Counter *****")

        if api_url:
            connection_config = SandboxDirectConnectionConfig(
                api_url=api_url,
                server_port=server_port
            )
        else:
            connection_config = SandboxLocalTunnelConnectionConfig(
                server_port=server_port
            )

        client = PodSnapshotSandboxClient(connection_config=connection_config)

        print("\n======= Testing Pod Snapshot Extension =======")
        
        sandbox = client.create_sandbox(template_name, namespace=namespace)

        time.sleep(WAIT_TIME_SECONDS)
        print(
            f"Creating first pod snapshot '{first_snapshot_name}' after {WAIT_TIME_SECONDS} seconds..."
        )
        snapshot_response = sandbox.snapshots.create(first_snapshot_name)
        test_snapshot_response(snapshot_response, first_snapshot_name)

        time.sleep(WAIT_TIME_SECONDS)

        print(
            f"\nCreating second pod snapshot '{second_snapshot_name}' after {WAIT_TIME_SECONDS} seconds..."
        )
        snapshot_response = sandbox.snapshots.create(second_snapshot_name)
        test_snapshot_response(snapshot_response, second_snapshot_name)
        recent_snapshot_uid = snapshot_response.snapshot_uid
        print(f"Recent snapshot UID: {recent_snapshot_uid}")

        # Wait a moment for the PodSnapshotPolicy controller's cache to recognize the new snapshot as the latest
        time.sleep(WAIT_TIME_SECONDS)
        
        print(f"\nChecking if sandbox was restored from snapshot '{recent_snapshot_uid}'...")
        restored_sandbox = client.create_sandbox(template_name, namespace=namespace)
        restore_result = restored_sandbox.is_restored_from_snapshot(recent_snapshot_uid)
        assert restore_result.success, restore_result.error_reason
        print("Pod was restored from the most recent snapshot.")

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
