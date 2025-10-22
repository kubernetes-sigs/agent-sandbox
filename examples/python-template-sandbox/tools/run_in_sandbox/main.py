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
import sys
import os

from agentic_sandbox.sandbox import Sandbox

def run_in_sandbox(code: str):
    """
    Executes a string of Python code inside a new, isolated sandbox.

    Args:
        code: The Python code to execute.
    """
    template_name = "python-sandbox-template"
    namespace = "default"
    
    print("--- Setting up isolated sandbox ---")
    
    try:
        with Sandbox(template_name, namespace) as sandbox:
            print("--- Sandbox is ready. Executing code... ---")
            
            # Write the code to a file in the sandbox
            sandbox.write("script_to_run.py", code)
            
            # Execute the script
            result = sandbox.run("python script_to_run.py")
            
            print("\n--- Execution Output ---")
            if result.stdout:
                print("--- STDOUT ---")
                print(result.stdout)
            if result.stderr:
                print("--- STDERR ---")
                print(result.stderr)
            
            print(f"--- Exit Code: {result.exit_code} ---")

    except Exception as e:
        print(f"\n--- An error occurred: {e} ---", file=sys.stderr)
        sys.exit(1)
    finally:
        print("\n--- Sandbox has been cleaned up ---")

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Run Python code in an isolated sandbox.")
    parser.add_argument(
        "code",
        type=str,
        help="The Python code to execute."
    )
    args = parser.parse_args()
    
    run_in_sandbox(args.code)
