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
from unittest.mock import MagicMock
from pydantic import ValidationError
from agentic_sandbox import SandboxClient
from agentic_sandbox.sandbox_client import ExecutionResult, FileEntry

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

            # Test list and exists
            print("\n--- Testing List and Exists ---")
            print(f"Checking if '{file_path}' exists...")
            exists = sandbox.exists(file_path)
            assert exists is True, f"Expected '{file_path}' to exist"

            print("Checking if 'non_existent_file.txt' exists...")
            not_exists = sandbox.exists("non_existent_file.txt")
            assert not_exists is False, "Expected 'non_existent_file.txt' to not exist"

            print("Listing files in '.' ...")
            files = sandbox.list(".")
            print(f"Files found: {[f.name for f in files]}")

            found = any(f.name == file_path for f in files)
            assert found is True, f"Expected '{file_path}' to be in the file list"

            file_entry = next(f for f in files if f.name == file_path)
            assert file_entry.size == len(file_content), f"Expected size {len(file_content)}, got {file_entry.size}"
            print("--- List and Exists Test Passed! ---")

            # Test introspection commands
            print("\n--- Testing Pod Introspection ---")

            print("\n--- Listing files in /app ---")
            list_files_result = sandbox.run("ls -la /app")
            print(list_files_result.stdout)

            print("\n--- Printing environment variables ---")
            env_result = sandbox.run("env")
            print(env_result.stdout)

            print("--- Introspection Tests Finished ---")
            
            print("\n--- Testing Pydantic Validation ---")
            
            # Test: ExecutionResult defaults (partial response)
            original_request = sandbox._request
            
            mock_response = MagicMock()
            mock_response.json.return_value = {} # Empty response
            sandbox._request = MagicMock(return_value=mock_response)
            
            print("Testing ExecutionResult defaults with empty response...")
            # This should not raise error because of defaults
            res = sandbox.run("echo test") 
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
                sandbox.list(".")
                raise AssertionError("ValidationError not raised for invalid FileEntry type")
            except ValidationError as e:
                print(f"Caught expected ValidationError: {e}")
            
            # Restore original method
            sandbox._request = original_request
            print("--- Pydantic Validation Tests Passed ---")

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
