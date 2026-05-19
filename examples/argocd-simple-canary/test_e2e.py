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
This is an end-to-end test for the SDK-based canary rollout.
It applies the pools and config, simulates traffic at 20% and 80% canary weights,
runs the analysis job, and verifies that traffic distribution matches the configuration.
"""

import subprocess
import time
import sys
import shlex
from sdk_router import acquire_sandbox, get_routing_config

def run_command(cmd, allow_fail=False):
    """Helper to run kubectl commands without shell=True"""
    print(f"[Exec] {cmd}")
    args = shlex.split(cmd)
    result = subprocess.run(args, capture_output=True, text=True)
    if result.returncode != 0:
        print(f"Error: {result.stderr.strip()}")
        if not allow_fail:
            raise Exception(f"Command failed: {cmd}")
    else:
        print(f"Result: {result.stdout.strip()}")
    return result

def get_assigned_sandbox(claim_name):
    # Reverted to .status.sandbox.name as seen in actual cluster output
    cmd = f"kubectl get sandboxclaim {claim_name} -n default -o jsonpath={{.status.sandbox.name}}"
    args = shlex.split(cmd)
    for _ in range(30): # Wait up to 30 seconds
        process = subprocess.run(args, capture_output=True, text=True)
        output = process.stdout.strip()
        if output:
            return output
        time.sleep(1)
    return None

def simulate_traffic(expected_percentage, count=5, use_go=False):
    """Creates 'count' claims and measures distribution based on actual assignment"""
    print(f"\n--- Creating {count} claims with target {expected_percentage}% Canary (Using {'Go' if use_go else 'Python'}) ---")
    
    cfg = get_routing_config()
    primary_pool = cfg["primary_pool"]
    canary_pool = cfg["canary_pool"]
    
    results = {primary_pool: 0, canary_pool: 0}
    claims = []
    
    for i in range(count):
        claim_name = f"argocd-test-claim-{expected_percentage}-{i}"
        if use_go:
            print(f"[Exec] go run main.go {claim_name}")
            process = subprocess.run(["go", "run", "main.go", claim_name], capture_output=True, text=True)
            print(f"Result: {process.stdout.strip()}")
            if process.returncode != 0:
                raise Exception(f"Failed to create claim via Go: {process.stderr}")
            
            if "[CANARY]" in process.stdout:
                selected_pool = canary_pool
            else:
                selected_pool = primary_pool
            
            claims.append((claim_name, selected_pool))
        else:
            selected_pool = acquire_sandbox(claim_name)
            if not selected_pool:
                raise Exception(f"Failed to create claim via Python for {claim_name}")
            claims.append((claim_name, selected_pool))
            
    print("Waiting for claims to be bound...")
    time.sleep(2)
    
    for claim_name, selected_pool in claims:
        sandbox = get_assigned_sandbox(claim_name)
        if not sandbox:
            raise Exception(f"Claim {claim_name} timed out waiting for sandbox.")
            
        print(f"Claim {claim_name} intended for {selected_pool}, bound to sandbox {sandbox}")
        
        # Measure actual assignment based on sandbox name prefix
        if sandbox.startswith(canary_pool):
            results[canary_pool] += 1
        elif sandbox.startswith(primary_pool):
            results[primary_pool] += 1
        else:
            print(f"Warning: Sandbox {sandbox} does not match either pool prefix.")
            
    # Since we fail fast, all claims must have succeeded if we reached here
    primary_pct = (results[primary_pool] / count) * 100
    canary_pct = (results[canary_pool] / count) * 100
    
    print(f"Actual Distribution: {primary_pool}: {results[primary_pool]} ({primary_pct}%), {canary_pool}: {results[canary_pool]} ({canary_pct}%)")
    return canary_pct

def main():
    print("Starting E2E SDK Routing Canary Simulation...\n")

    # 1. Apply prerequisites
    run_command("kubectl apply -f templates.yaml")
    run_command("kubectl apply -f pools.yaml")
    run_command("kubectl apply -f canary-config.yaml")
    time.sleep(2)

    # 2. Test Phase 1: Set to 20%
    run_command("kubectl patch configmap canary-routing-config --type=merge -p '{\"data\":{\"canary_percentage\":\"20\"}}'")
    time.sleep(2)
    v2_pct_20 = simulate_traffic(20, count=5)

    # Run analysis job to verify claims are ready
    print("\n--- Running Analysis Job ---")
    import analysis_job
    analysis_success = analysis_job.check_claims(timeout_seconds=10)
    if analysis_success:
        print("Analysis Result: PASSED")
    else:
        print("Analysis Result: FAILED")

    # 3. Test Phase 2: Set to 80%
    run_command("kubectl patch configmap canary-routing-config --type=merge -p '{\"data\":{\"canary_percentage\":\"80\"}}'")
    time.sleep(2)
    v2_pct_80 = simulate_traffic(80, count=5)

    # NEW: Test Go version at 50%
    print("\n--- Testing Go Version at 50% Canary ---")
    run_command("kubectl patch configmap canary-routing-config --type=merge -p '{\"data\":{\"canary_percentage\":\"50\"}}'")
    time.sleep(2)
    v2_pct_50_go = simulate_traffic(50, count=5, use_go=True)

    # 4. Test Phase 3: Set to 100% (Full Rollout)
    print("\n--- Phase 3: Advancing to 100% Canary (Full Rollout) ---")
    run_command("kubectl patch configmap canary-routing-config --type=merge -p '{\"data\":{\"canary_percentage\":\"100\"}}'")
    time.sleep(2)
    v2_pct_100 = simulate_traffic(100, count=5)

    # 5. Cleanup v1 Pool (Simulating post-rollout cleanup)
    print("\n--- Phase 4: Rollout Complete. Removing Old WarmPool (v1) ---")
    print("Simulating Argo CD removing the old pool from Git...")
    run_command("kubectl delete sandboxwarmpool python-pool-v1")
    
    print("Verifying that claims now only go to v2...")
    v2_pct_post_cleanup = simulate_traffic(100, count=5)

    print("\n================ E2E TEST SUMMARY ================")
    print(f"Config @ 20%  -> Measured Canary Traffic: {v2_pct_20}%")
    print(f"Config @ 80%  -> Measured Canary Traffic: {v2_pct_80}%")
    print(f"Config @ 50% (Go) -> Measured Canary Traffic: {v2_pct_50_go}%")
    print(f"Config @ 100% -> Measured Canary Traffic: {v2_pct_100}%")
    print(f"Post-Cleanup  -> Measured Canary Traffic: {v2_pct_post_cleanup}%")
    print(f"Analysis Job Result: {'PASSED' if analysis_success else 'FAILED'}")
    
    # Cleanup test claims
    print("\nCleaning up test claims...")
    run_command("kubectl delete sandboxclaim -l app=argocd-sdk-test")
    
    # Restore ConfigMap to default for future runs
    print("Restoring ConfigMap to default (20%)...")
    run_command("kubectl patch configmap canary-routing-config --type=merge -p '{\"data\":{\"canary_percentage\":\"20\"}}'")
    
    # Re-apply pools.yaml to restore v1 pool for future runs
    print("Restoring WarmPools...")
    run_command("kubectl apply -f pools.yaml")

    if v2_pct_80 > v2_pct_20 and v2_pct_100 == 100.0 and v2_pct_post_cleanup == 100.0 and analysis_success:
        print("\nSUCCESS: Complete canary scenario validated successfully!")
        return 0
    else:
        print("\nWARNING: Test failed or conditions not met.")
        return 1

if __name__ == "__main__":
    sys.exit(main())
