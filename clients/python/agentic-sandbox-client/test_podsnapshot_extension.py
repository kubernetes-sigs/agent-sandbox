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
from agentic_sandbox.gke_extensions import PodSnapshotSandboxClient


def test_checkpoint_response(checkpoint_response, checkpoint_name):
    assert hasattr(
        checkpoint_response, "trigger_name"
    ), "Checkpoint response missing 'trigger_name' attribute"

    print(f"Trigger Name: {checkpoint_response.trigger_name}")
    print(f"Success: {checkpoint_response.success}")
    print(f"Error Code: {checkpoint_response.error_code}")
    print(f"Error Reason: {checkpoint_response.error_reason}")

    assert checkpoint_response.trigger_name.startswith(
        checkpoint_name
    ), f"Expected trigger name prefix '{checkpoint_name}', but got '{checkpoint_response.trigger_name}'"
    assert (
        checkpoint_response.success
    ), f"Expected success=True, but got False. Reason: {checkpoint_response.error_reason}"
    assert checkpoint_response.error_code == 0


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
    first_checkpoint_name = "test-snapshot-10"
    second_checkpoint_name = "test-snapshot-20"
    policy_name = "example-psp-workload"
    core_v1_api = client.CoreV1Api()

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
                f"Creating first pod snapshot '{first_checkpoint_name}' after {wait_time} seconds..."
            )
            checkpoint_response = sandbox.checkpoint(first_checkpoint_name)
            test_checkpoint_response(checkpoint_response, first_checkpoint_name)

            time.sleep(wait_time)

            print(
                f"\nCreating second pod snapshot '{second_checkpoint_name}' after {wait_time} seconds..."
            )
            checkpoint_response = sandbox.checkpoint(second_checkpoint_name)
            test_checkpoint_response(checkpoint_response, second_checkpoint_name)

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
        ) as sandbox_restored:  # restores from second_checkpoint_name by default

            print("\nWaiting 5 seconds for restored pod to resume printing...")
            time.sleep(5)

            # Fetch logs using the Kubernetes API
            logs = core_v1_api.read_namespaced_pod_log(
                name=sandbox_restored.pod_name, namespace=sandbox_restored.namespace
            )

            # Extract the sequence of 'Count:' values from the pod logs
            counts = [int(n) for n in re.findall(r"Count: (\d+)", logs)]
            assert (
                len(counts) > 0
            ), "Failed to retrieve any 'Count:' logs from restored pod."

            # Verify the counter resumed from the correct checkpoint state.
            # The second snapshot was taken after two wait intervals (totaling 20s if wait_time=10).
            min_expected_count_at_restore = wait_time * 2
            first_count_after_restore = counts[0]

            print(f"First count after restore: {first_count_after_restore}")

            assert first_count_after_restore >= min_expected_count_at_restore, (
                f"State Mismatch! Expected counter to start >= {min_expected_count_at_restore}, "
                f"but got {first_count_after_restore}. The pod likely restarted from scratch."
            )

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
