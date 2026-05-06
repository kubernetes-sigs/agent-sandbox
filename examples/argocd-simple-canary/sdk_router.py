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
This script demonstrates client-side stochastic routing.
It reads the canary-routing-config ConfigMap to determine which SandboxWarmPool to target
when creating a SandboxClaim.
"""

from kubernetes import client, config
import random
import time
import subprocess
import sys

def get_routing_config():
    """
    Fetches the canary routing config from the Kubernetes ConfigMap.
    This ConfigMap is what Argo CD manages. Changing the YAML in Git results
    in Argo CD updating this ConfigMap, which the SDK immediately picks up.
    """
    try:
        # Tries to load local kubeconfig for testing
        config.load_kube_config()
    except Exception:
        # Falls back to in-cluster service account when deployed inside K8s
        config.load_incluster_config()

    v1 = client.CoreV1Api()
    try:
        config_map = v1.read_namespaced_config_map(name="canary-routing-config", namespace="default")
        data = config_map.data
        cfg = {
            "primary_pool": data.get("primary_pool", "python-pool-v1"),
            "canary_pool": data.get("canary_pool", "python-pool-v2"),
            "primary_template": data.get("primary_template", "sandbox-python-template-v1"),
            "canary_template": data.get("canary_template", "sandbox-python-template-v2"),
            "canary_percentage": int(data.get("canary_percentage", "0"))
        }
        if cfg["canary_percentage"] < 0 or cfg["canary_percentage"] > 100:
            raise ValueError(f"Invalid canary percentage: {cfg['canary_percentage']}. Must be between 0 and 100.")
        return cfg
    except Exception as e:
        print(f"Error reading configmap, defaulting to primary pool. Error: {e}")
        return {
            "primary_pool": "python-pool-v1", 
            "canary_pool": "python-pool-v2",
            "primary_template": "sandbox-python-template-v1",
            "canary_template": "sandbox-python-template-v2",
            "canary_percentage": 0
        }

def acquire_sandbox(claim_name):
    cfg = get_routing_config()
    
    # Weighted stochastic routing
    if random.randint(1, 100) <= cfg["canary_percentage"]:
        selected_pool = cfg["canary_pool"]
        selected_template = cfg["canary_template"]
        print(f"[CANARY] Random choice selected! Routing claim to -> {selected_pool} (Canary) using template {selected_template}")
    else:
        selected_pool = cfg["primary_pool"]
        selected_template = cfg["primary_template"]
        print(f"[PRIMARY] Routing claim to -> {selected_pool} (Stable) using template {selected_template}")
        
    # Create actual SandboxClaim
    yaml_content = f"""
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: {claim_name}
  namespace: default
  labels:
    app: argocd-sdk-test
spec:
  sandboxTemplateRef:
    name: {selected_template}
  warmpool: {selected_pool}
"""
    process = subprocess.Popen(["kubectl", "apply", "-f", "-"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    stdout, stderr = process.communicate(input=yaml_content)
    if process.returncode != 0:
        print(f"Failed to create claim {claim_name}: {stderr}")
        return None
    
    return selected_pool

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: python sdk_router.py <claim_name>")
        sys.exit(1)
        
    claim_name = sys.argv[1]
    print(f"Creating Sandbox Claim '{claim_name}' using SDK routing reading from Cluster ConfigMap...\n")
    acquire_sandbox(claim_name)
