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
from asyncio import sleep
from agentic_sandbox import SandboxClient

async def main(template_name: str):
    """
    Tests the Sandbox client by creating a sandbox, running a command,
    and then cleaning up.
    """
    namespace = "default"
    
    print("--- Starting Sandbox Client Test ---")
    
    try:
        with SandboxClient(template_name, namespace) as sandbox:
            await sleep(2)  # Wait for the sandbox to be fully ready
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
    args = parser.parse_args()
    asyncio.run(main(template_name=args.template_name))
