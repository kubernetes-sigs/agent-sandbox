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

import argparse
import asyncio
import time
import sys
from kubernetes import client, config
import re
from k8s_agent_sandbox.gke_extensions import PodSnapshotSandboxClient


def test_snapshot_response(snapshot_response, snapshot_name):
    assert hasattr(
        snapshot_response, "trigger_name"
    ), "snapshot response missing 'trigger_name' attribute"

    print(f"Trigger Name: {snapshot_response.trigger_name}")
    print(f"Snapshot UID: {snapshot_response.snapshot_uid}")
    print(f"Success: {snapshot_response.success}")
    print(f"Error Code: {snapshot_response.error_code}")
    print(f"Error Reason: {snapshot_response.error_reason}")

    assert snapshot_response.trigger_name.startswith(
        snapshot_name
    ), f"Expected trigger name prefix '{snapshot_name}', but got '{snapshot_response.trigger_name}'"
    assert (
        snapshot_response.success
    ), f"Expected success=True, but got False. Reason: {snapshot_response.error_reason}"
    assert snapshot_response.error_code == 0


async def main(
    template_name: str,
    api_url: str | None,
    namespace: str,
    server_port: int,
    labels: dict[str, str],
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

    wait_time = 10
    first_snapshot_name = "test-snapshot-10"
    second_snapshot_name = "test-snapshot-20"

    try:
        print("\n***** Phase 1: Starting Counter *****")

        with PodSnapshotSandboxClient(
            template_name=template_name,
            namespace=namespace,
            api_url=api_url,
            server_port=server_port,
        ) as sandbox:
            print("\n======= Testing Pod Snapshot Extension =======")
            assert sandbox.controller_ready == True, "Sandbox controller is not ready."

            time.sleep(wait_time)
            print(
                f"Creating first pod snapshot '{first_snapshot_name}' after {wait_time} seconds..."
            )
            snapshot_response = sandbox.snapshot(first_snapshot_name)
            test_snapshot_response(snapshot_response, first_snapshot_name)

            time.sleep(wait_time)

            print(
                f"\nCreating second pod snapshot '{second_snapshot_name}' after {wait_time} seconds..."
            )
            snapshot_response = sandbox.snapshot(second_snapshot_name)
            test_snapshot_response(snapshot_response, second_snapshot_name)
            recent_snapshot_uid = snapshot_response.snapshot_uid
            print(f"Recent snapshot UID: {recent_snapshot_uid}")

            print("\n***** List all existing ready snapshots with the policy name. *****") 
            snapshots = sandbox.list_snapshots(policy_name=policy_name)
            for snap in snapshots:
                print(f"Snapshot ID: {snap['snapshot_id']}, Triggered By: {snap['trigger_name']}, Source Pod: {snap['source_pod']}, Creation Time: {snap['creationTimestamp']}, Policy Name: {snap['policy_name']}")

        print("\n***** Phase 2: Restoring from most recent snapshot & Verifying *****")
        with PodSnapshotSandboxClient(
            template_name=template_name,
            namespace=namespace,
            api_url=api_url,
            server_port=server_port,
        ) as sandbox_restored:  # restores from second_snapshot_name by default

            print("\nWaiting 5 seconds for restored pod to resume printing...")
            time.sleep(5)

            restore_result = sandbox_restored.is_restored_from_snapshot(
                recent_snapshot_uid
            )
            assert restore_result.success, "Pod was not restored from a snapshot."
            print("Pod was restored from the most recent snapshot.")

            print("\n**** Deleting snapshots *****")
            deleted_snapshots = sandbox_restored.delete_snapshots()
            print(f"Deleted Snapshots: {deleted_snapshots}")

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

    # Default is None to allow testing the Port-Forward fallback
    parser.add_argument(
        "--gateway-name",
        default=None,
        help="The name of the Gateway resource. If omitted, defaults to local port-forward mode.",
    )

    parser.add_argument(
        "--gateway-namespace",
        default=None,
        help="The namespace of the Gateway resource. If omitted, defaults to local port-forward mode.",
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
    parser.add_argument(
        "--labels",
        nargs="+",
        default=["app=sandbox-test"],
        help="Labels for the sandbox pod/claim in key=value format (e.g. app=sandbox-test env=dev)",
    )

    args = parser.parse_args()

    labels_dict = {}
    for l in args.labels:
        if "=" in l:
            k, v = l.split("=", 1)
            labels_dict[k] = v
        else:
            print(f"Warning: Ignoring invalid label format '{l}'. Use key=value.")

    asyncio.run(
        main(
            template_name=args.template_name,
            api_url=args.api_url,
            namespace=args.namespace,
            server_port=args.server_port,
            labels=labels_dict,
        )
    )
