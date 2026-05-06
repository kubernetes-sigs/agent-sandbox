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

"""
This script simulates an Argo Rollouts Analysis Job.
It checks if all active SandboxClaims created by the test are successfully bound and Ready.
It exits with 0 on success and 1 on failure.
"""

import subprocess
import sys
import time
import shlex

def run_command(cmd):
    args = shlex.split(cmd)
    result = subprocess.run(args, capture_output=True, text=True)
    return result

def check_claims(timeout_seconds=30):
    print(f"Starting analysis check on SandboxClaims...")
    
    # Give the controller a few seconds to process claims created by the app
    time.sleep(5)
    
    # Added -n default
    cmd = "kubectl get sandboxclaim -n default -l app=argocd-sdk-test -o json"
    
    start_time = time.time()
    while time.time() - start_time < timeout_seconds:
        result = run_command(cmd)
        if result.returncode != 0:
            print(f"Failed to get claims: {result.stderr}")
            return False
            
        # If no claims found, we consider it a pass (maybe no traffic yet)
        if not result.stdout.strip() or result.stdout.strip() == "{}":
            print("No active test claims found to analyze.")
            return True
            
        import json
        try:
            claims = json.loads(result.stdout)
            items = claims.get("items", [])
            
            if not items:
                print("No items in claim list.")
                return True
                
            all_ready = True
            for item in items:
                status = item.get("status", {})
                conditions = status.get("conditions", [])
                
                ready = False
                for cond in conditions:
                    if cond.get("type") == "Ready" and cond.get("status") == "True":
                        ready = True
                        break
                        
                if not ready:
                    print(f"Claim {item['metadata']['name']} is not ready yet.")
                    all_ready = False
                    break
                    
            if all_ready:
                print("All active test claims are successfully bound and Ready!")
                return True
                
        except Exception as e:
            print(f"Error parsing JSON: {e}")
            return False
            
        print("Waiting for all claims to become ready...")
        time.sleep(2)
        
    print("Timed out waiting for claims to become ready.")
    return False

if __name__ == "__main__":
    # This script will exit with 0 on success, and 1 on failure.
    # Argo Rollouts Job-based analysis uses this exit code to determine health.
    success = check_claims()
    if success:
        print("Analysis PASSED")
        sys.exit(0)
    else:
        print("Analysis FAILED")
        sys.exit(1)
