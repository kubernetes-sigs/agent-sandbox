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
from agentic_sandbox import SandboxClient

POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name"


async def main(template_name: str, gateway_name: str | None, api_url: str | None, namespace: str,
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

    try:
        # Initialize Client with Keyword Arguments for safety
        with SandboxClient(
            template_name=template_name,
            namespace=namespace,
            gateway_name=gateway_name,
            api_url=api_url,
            server_port=server_port,
            enable_tracing=enable_tracing
        ) as sandbox:

            print("\n--- Testing Pod Name Discovery ---")
            assert sandbox.annotations is not None, "Sandbox annotations were not stored on the client"

            pod_name_annotation = sandbox.annotations.get(POD_NAME_ANNOTATION)

            if pod_name_annotation:
                print(f"Found pod name from annotation: {pod_name_annotation}")
                assert sandbox.pod_name == pod_name_annotation, f"Expected pod_name to be '{pod_name_annotation}', but got '{sandbox.pod_name}'"
                print("--- Pod Name Discovery Test Passed (Annotation) ---")
            else:
                print("Pod name annotation not found, falling back to sandbox name.")
                assert sandbox.pod_name == sandbox.sandbox_name, f"Expected pod_name to be '{sandbox.sandbox_name}', but got '{sandbox.pod_name}'"
                print("--- Pod Name Discovery Test Passed (Fallback) ---")

            print("\n--- Testing Command Execution ---")
            command_to_run = "echo 'Hello from the sandbox!'"
            print(f"Executing command: '{command_to_run}'")

            result = sandbox.run(command_to_run)

            print(f"Stdout: {result.stdout.strip()}")
            print(f"Stderr: {result.stderr.strip()}")
            print(f"Exit Code: {result.exit_code}")

            assert result.exit_code == 0
            assert result.stdout.strip() == "Hello from the sandbox!"

            print("\n--- Command Execution Test Passed! ---")

            # Test file operations
            print("\n--- Testing File Operations ---")
            file_content = "This is a test file."
            file_path = "test.txt"

            print(f"Writing content to '{file_path}'...")
            sandbox.write(file_path, file_content)

            print(f"Reading content from '{file_path}'...")
            read_content = sandbox.read(file_path).decode('utf-8')

            print(f"Read content: '{read_content}'")
            assert read_content == file_content
            print("--- File Operations Test Passed! ---")

            # Test introspection commands
            print("\n--- Testing Pod Introspection ---")

            print("\n--- Listing files in /app ---")
            list_files_result = sandbox.run("ls -la /app")
            print(list_files_result.stdout)

            print("\n--- Printing environment variables ---")
            env_result = sandbox.run("env")
            print(env_result.stdout)

            print("--- Introspection Tests Finished ---")

            # Test pause and resume
            print("\n--- Testing Pause/Resume ---")

            print("Pausing sandbox...")
            sandbox.pause()
            print("Sandbox paused successfully.")

            print("Resuming sandbox (with wait_for_ready)...")
            sandbox.resume()  # wait=True by default, blocks until pod is ready
            print("Sandbox resumed and ready.")

            print("Running command after resume...")
            resume_result = sandbox.run("echo 'Back after resume!'")
            print(f"Stdout: {resume_result.stdout.strip()}")
            print(f"Exit Code: {resume_result.exit_code}")
            assert resume_result.exit_code == 0
            assert resume_result.stdout.strip() == "Back after resume!"
            print("--- Pause/Resume Test Passed! ---")

            # Test data persistence across pause/resume
            # Without PVCs, ephemeral storage is lost when the pod is deleted.
            # The Sandbox spec supports volumeClaimTemplates for persistence,
            # but this is not exposed through SandboxTemplate/SandboxClaim.
            # TODO Add support for PVC-backed storage in SandboxTemplate/SandboxClaim
            print("\n--- Testing Ephemeral Storage Across Pause/Resume ---")

            persist_path = "persist_test.txt"
            persist_content = "data before pause"
            print(f"Writing '{persist_content}' to '{persist_path}'...")
            sandbox.write(persist_path, persist_content)

            # Verify the file exists before pause
            read_back = sandbox.read(persist_path).decode("utf-8")
            assert read_back == persist_content, f"Pre-pause read failed: '{read_back}'"
            print("File confirmed written before pause.")

            print("Pausing sandbox...")
            sandbox.pause()
            # Wait for the old pod to be fully terminated before resuming,
            # otherwise the router may still route to the dying pod.
            time.sleep(3)

            print("Resuming sandbox...")
            sandbox.resume()

            print(f"Checking if '{persist_path}' survived pause/resume...")
            check_result = sandbox.run(f"cat /app/{persist_path}")
            print(f"cat exit_code: {check_result.exit_code}")
            print(f"cat stdout: '{check_result.stdout.strip()}'")
            print(f"cat stderr: '{check_result.stderr.strip()}'")

            assert check_result.exit_code != 0, (
                f"Expected ephemeral file to be lost after pause/resume, "
                f"but it still exists with content: '{check_result.stdout.strip()}'"
            )
            print("Confirmed: ephemeral storage is NOT persisted across pause/resume.")
            print("--- Ephemeral Storage Test Passed! ---")

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

    asyncio.run(main(
        template_name=args.template_name,
        gateway_name=args.gateway_name,
        api_url=args.api_url,
        namespace=args.namespace,
        server_port=args.server_port,
        enable_tracing=args.enable_tracing
    ))
