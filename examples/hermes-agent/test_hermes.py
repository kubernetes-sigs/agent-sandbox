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

import subprocess
import time
import urllib.request
import json
import sys
import os

def get_pod_name():
    try:
        # Use local kubeconfig file if present
        cmd = ["kubectl"]
        if os.path.exists("kubeconfig"):
            cmd.append("--kubeconfig=kubeconfig")
        cmd.extend(["get", "pods", "-o", "name"])
        
        output = subprocess.check_output(cmd, text=True)
        for line in output.strip().split('\n'):
            if "hermes-agent" in line:
                return line.strip().replace("pod/", "")
    except Exception as e:
        print(f"Error getting pod name: {e}")
        return None
    return None

def test_hermes():
    pod_name = get_pod_name()
    if not pod_name:
        print("Failed to find hermes-agent pod.")
        sys.exit(1)
    
    print(f"Found pod: {pod_name}")
    
    # Start port forwarding using local kubeconfig if present
    cmd = ["kubectl"]
    if os.path.exists("kubeconfig"):
        cmd.append("--kubeconfig=kubeconfig")
    cmd.extend(["port-forward", f"pod/{pod_name}", "8642:8642"])
    
    pf_process = subprocess.Popen(cmd)
    
    success = False
    try:
        print("Waiting for port-forwarding to be ready...")
        time.sleep(5) # Give it some time to establish
        
        url = "http://localhost:8642/v1/models"
        print(f"Querying {url}...")
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req, timeout=5) as response:
            if response.status == 200:
                data = json.loads(response.read().decode())
                print(f"Success! Response: {data}")
                success = True
            else:
                print(f"Failed with status code: {response.status}")
    except Exception as e:
        print(f"Connection failed: {e}")
    finally:
        print("Cleaning up port-forwarding...")
        pf_process.terminate()
        pf_process.wait()
        
    if success:
        print("Test Passed!")
        sys.exit(0)
    else:
        print("Test Failed!")
        sys.exit(1)

if __name__ == "__main__":
    test_hermes()
