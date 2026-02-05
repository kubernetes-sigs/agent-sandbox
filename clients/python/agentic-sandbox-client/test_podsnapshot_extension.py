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
import asyncio
import time
import sys
from kubernetes import client, config
import re
from agentic_sandbox.gke_extensions import PodSnapshotSandboxClient

POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name"


async def main(template_name: str, api_url: str | None, namespace: str, server_port: int, labels: dict[str, str]):
    """
    Tests the Sandbox client by creating a sandbox, running a command,
    and then cleaning up.
    """

    print(
        f"--- Starting Sandbox Client Test (Namespace: {namespace}, Port: {server_port}) ---")

    # Load kube config
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

    wait_time = 10
    first_checkpoint_name = "test-snapshot-10"
    second_checkpoint_name = "test-snapshot-20"
    v1 = client.CoreV1Api()

    try:
        print("\n***** Phase 1: Starting Counter *****")

        with PodSnapshotSandboxClient(
            template_name=template_name,
            namespace=namespace,
            api_url=api_url,
            server_port=server_port
        ) as sandbox:
            print("\n======= Testing Pod Snapshot Extension =======")
            assert sandbox.controller_ready == True, "Sandbox controller is not ready."

            time.sleep(wait_time)
            print(f"Creating first pod snapshot '{first_checkpoint_name}' after {wait_time} seconds...")
            snapshot_result, trigger_name = sandbox.checkpoint(first_checkpoint_name)
            print(f"Trigger Command Stdout: {snapshot_result.stdout.strip()}")
            print(f"Trigger Command Stderr: {snapshot_result.stderr.strip()}")
            print(f"Trigger Command Exit Code: {snapshot_result.exit_code}")

            assert snapshot_result.exit_code == 0

            time.sleep(wait_time)

            print(f"\nCreating second pod snapshot '{second_checkpoint_name}' after {wait_time} seconds...")
            snapshot_result, trigger_name = sandbox.checkpoint(second_checkpoint_name)
            print(f"Trigger Command Stdout: {snapshot_result.stdout.strip()}")
            print(f"Trigger Command Stderr: {snapshot_result.stderr.strip()}")
            print(f"Trigger Command Exit Code: {snapshot_result.exit_code}")

            assert snapshot_result.exit_code == 0


        print("\n***** Phase 2: Restoring from most recent snapshot & Verifying *****")
        with PodSnapshotSandboxClient(
            template_name=template_name,
            namespace=namespace,
            api_url=api_url,
            server_port=server_port
        ) as sandbox_restored: # restores from second_snapshot_name by default

            print("\nWaiting 5 seconds for restored pod to resume printing...")
            time.sleep(5)

            # Fetch logs using the Kubernetes API 
            logs = v1.read_namespaced_pod_log(
                name=sandbox_restored.pod_name, 
                namespace=sandbox_restored.namespace
            )

            # logs must not be empty     
            counts = [int(n) for n in re.findall(r"Count: (\d+)", logs)]
            assert len(counts) > 0, "Failed to retrieve any 'Count:' logs from restored pod."

            # The first number printed by the restored pod must be >= 20.
            # If it restarted, it would be 0 or 1.
            first_count = counts[0]
            print("this is the first_count:", first_count)
            assert first_count >= wait_time * 2, (
                f"State Mismatch! Expected counter to start >= {wait_time*2}, "
                f"but got {first_count}. The pod likely restarted from scratch."
            )

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
        help="The name of the sandbox template to use for the test."
    )

    # Default is None to allow testing the Port-Forward fallback
    parser.add_argument(
        "--gateway-name",
        default=None,
        help="The name of the Gateway resource. If omitted, defaults to local port-forward mode."
    )

    parser.add_argument(
        "--gateway-namespace",
        default=None,
        help="The namespace of the Gateway resource. If omitted, defaults to local port-forward mode."
    )

    parser.add_argument(
        "--api-url", help="Direct URL to router (e.g. http://localhost:8080)", default=None)
    parser.add_argument("--namespace", default="default",
                        help="Namespace to create sandbox in")
    parser.add_argument("--server-port", type=int, default=8888,
                        help="Port the sandbox container listens on")
    parser.add_argument("--labels", nargs="+", default=["app=sandbox-test"],
                        help="Labels for the sandbox pod/claim in key=value format (e.g. app=sandbox-test env=dev)")

    args = parser.parse_args()

    labels_dict = {}
    for l in args.labels:
        if "=" in l:
            k, v = l.split("=", 1)
            labels_dict[k] = v
        else:
            print(f"Warning: Ignoring invalid label format '{l}'. Use key=value.")

    asyncio.run(main(
        template_name=args.template_name,
        api_url=args.api_url,
        namespace=args.namespace,
        server_port=args.server_port,
        labels=labels_dict
    ))
# python3 test_podsnapshot_extension.py --labels app=agent-sandbox-workload --template-name python-counter-template --namespace sandbox-test
