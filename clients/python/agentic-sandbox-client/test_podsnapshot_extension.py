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
from kubernetes import config
from k8s_agent_sandbox.gke_extensions import PodSnapshotSandboxClient
from k8s_agent_sandbox.gke_extensions.podsnapshot_client import SnapshotResponse


WAIT_TIME_SECONDS = 10


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

    try:
        print("\n***** Phase 1: Starting Counter *****")

        with PodSnapshotSandboxClient(
            template_name=template_name,
            namespace=namespace,
            api_url=api_url,
            server_port=server_port,
        ) as sandbox:
            print("\n======= Testing Pod Snapshot Extension =======")
            assert sandbox.snapshot_crd_installed, "Pod Snapshot CRD is not installed."
            time.sleep(WAIT_TIME_SECONDS)
            print(
                f"Creating first pod snapshot '{first_snapshot_trigger_name}' after {WAIT_TIME_SECONDS} seconds..."
            )
            snapshot_response = sandbox.snapshot(
                trigger_name=first_snapshot_trigger_name
            )
            test_snapshot_response(snapshot_response, first_snapshot_trigger_name)
            first_snapshot_uid = snapshot_response.snapshot_uid
            print(f"First snapshot UID: {first_snapshot_uid}")

            time.sleep(WAIT_TIME_SECONDS)

            print(
                f"\nCreating second pod snapshot '{second_snapshot_trigger_name}' after {WAIT_TIME_SECONDS} seconds..."
            )
            snapshot_response = sandbox.snapshot(
                trigger_name=second_snapshot_trigger_name
            )
            test_snapshot_response(snapshot_response, second_snapshot_trigger_name)
            recent_snapshot_uid = snapshot_response.snapshot_uid
            print(f"Recent snapshot UID: {recent_snapshot_uid}")

            print(
                "\n***** List all existing ready snapshots associated with the sandbox. *****"
            )
            list_result = sandbox.list_snapshots(grouping_labels=grouping_labels)
            assert list_result.success, list_result.error_reason
            assert (
                len(list_result.snapshots) == 2
            ), f"Expected 2 snapshots, but got {len(list_result.snapshots)}"
            # list_snapshots returns snapshots sorted by creation timestamp descending
            assert (
                list_result.snapshots[0].snapshot_uid == recent_snapshot_uid
            ), f"Expected most recent snapshot UID to be {recent_snapshot_uid}, but got {list_result.snapshots[0].snapshot_uid}"
            assert (
                list_result.snapshots[1].snapshot_uid == first_snapshot_uid
            ), f"Expected oldest snapshot UID to be {first_snapshot_uid}, but got {list_result.snapshots[1].snapshot_uid}"

            for snap in list_result.snapshots:
                print(
                    f"Snapshot UID: {snap.snapshot_uid}, Source Pod: {snap.source_pod}, Creation Time: {snap.creation_timestamp}"
                )

            print(
                "\n***** Phase 2: Restoring from most recent snapshot & Verifying *****"
            )
            with PodSnapshotSandboxClient(
                template_name=template_name,
                namespace=namespace,
                api_url=api_url,
                server_port=server_port,
            ) as sandbox_restored:  # restores from latest snapshot by default

                restore_result = sandbox_restored.is_restored_from_snapshot(
                    recent_snapshot_uid
                )
                assert restore_result.success, restore_result.error_reason
                print("Pod was restored from the most recent snapshot.")

                print("\n**** Deleting snapshots *****")
                delete_result = sandbox.delete_snapshots(
                    grouping_labels=grouping_labels
                )
                assert delete_result.success, delete_result.error_reason
                assert (
                    len(delete_result.deleted_snapshots) == 2
                ), f"Expected 2 snapshots to be deleted, but got {len(delete_result.deleted_snapshots)}"
                assert (
                    delete_result.deleted_snapshots[0] == recent_snapshot_uid
                ), f"Expected most recent snapshot UID to be {recent_snapshot_uid}, but got {delete_result.deleted_snapshots[0]}"
                assert (
                    delete_result.deleted_snapshots[1] == first_snapshot_uid
                ), f"Expected oldest snapshot UID to be {first_snapshot_uid}, but got {delete_result.deleted_snapshots[1]}"
                print(f"Deleted Snapshots: {delete_result.deleted_snapshots}")

        print("--- Pod Snapshot Test Passed! ---")

    except Exception as e:
        print(f"\n--- An error occurred during the test: {e} ---")
        # The __exit__ method of the Sandbox class will handle cleanup.
    finally:
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
