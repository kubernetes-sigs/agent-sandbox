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

import subprocess
import time
import os
from agentic_sandbox.computer_use_sandbox import ComputerUseSandbox
from kubernetes import client, config

def main():
    """
    Tests the Sandbox client by creating a sandbox, running the tester.py script
    against it, and then cleaning up.
    """
    template_name = "sandbox-python-computeruse-template"
    namespace = "default"
    
    print("--- Starting Programmatic Sandbox Test ---")
    
    try:
        
        print("Passing GEMINI KEY")
        # Create the gemini-api-key secret
        subprocess.run(f"kubectl create secret generic gemini-api-key --from-literal=key={os.environ['GEMINI_API_KEY']} --dry-run=client -o yaml | kubectl apply -f -", shell=True)
        
        # Wait for the secret to be created
        config.load_kube_config()
        core_v1_api = client.CoreV1Api()
        for _ in range(10):
            try:
                core_v1_api.read_namespaced_secret("gemini-api-key", namespace)
                print("Secret is ready.")
                break
            except client.ApiException as e:
                if e.status == 404:
                    time.sleep(1)
                else:
                    raise
        else:
            raise TimeoutError("Secret did not become ready in time.")

        with ComputerUseSandbox(template_name, namespace) as sandbox:
            print("\n--- Sandbox is ready ---")
            
            # Run the tester.py script
            tester_command = ["python3", "tester.py", "127.0.0.1", str(sandbox.server_port)]
            print(f"Running tester: {' '.join(tester_command)}")
            
            result = subprocess.run(tester_command, capture_output=True, text=True)
            
            print("\n--- Tester Output ---")
            print(result.stdout)
            print(result.stderr)
            
            if result.returncode == 0:
                print("\n--- Programmatic Test Passed! ---")
            else:
                print("\n--- Programmatic Test Failed! ---")

    except Exception as e:
        print(f"\n--- An error occurred during the test: {e} ---")
    finally:
        print("\n--- Programmatic Sandbox Test Finished ---")

if __name__ == "__main__":
    main()