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

import sys
import logging
import socket
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxInClusterConnectionConfig

logging.basicConfig(level=logging.INFO)

def run_incluster_test(template_name, namespace):
    print(f"\n==================================================")
    print(f"Starting In-Cluster Compatibility and Direct DNS Routing E2E Test in namespace {namespace}...")
    print(f"==================================================")
    
    # Use standard default connection config (Cluster DNS)
    config = SandboxInClusterConnectionConfig()
    client = SandboxClient(connection_config=config)
    
    sandbox = None
    try:
        print(f"Creating sandbox from template '{template_name}'...")
        sandbox = client.create_sandbox(
            template=template_name,
            namespace=namespace,
            warmpool="none",
        )
        # Explicit DNS Resolution test on target service hostname
        dns_name = f"{sandbox.sandbox_id}.{namespace}.svc.cluster.local"
        print(f"Resolving direct DNS name inside the cluster: {dns_name}...")
        try:
            addr_info = socket.getaddrinfo(dns_name, None)
            resolved_ips = [info[4][0] for info in addr_info if info[4]]
            print(f"Successfully resolved direct DNS {dns_name} to IPs: {resolved_ips}")
            if not resolved_ips:
                raise RuntimeError("DNS resolution returned no IPs")
        except socket.gaierror as e:
            raise RuntimeError(f"Direct cluster DNS resolution failed for hostname {dns_name}: {e}") from e

        print("Executing test command inside sandbox...")
        res = sandbox.commands.run("echo 'Hello from In-Cluster!'")
        print(f"Stdout: {res.stdout}")
        print(f"Stderr: {res.stderr}")
        if "Hello from In-Cluster!" not in res.stdout:
            raise RuntimeError(f"Unexpected stdout: {res.stdout}")
        if res.exit_code != 0:
            raise RuntimeError(f"Unexpected exit code: {res.exit_code}")
        print("In-cluster compatibility and direct DNS routing E2E test completed successfully!")
    finally:
        if sandbox is not None:
            print("Terminating sandbox...")
            sandbox.terminate()
        client.delete_all()

if __name__ == "__main__":
    if len(sys.argv) < 3:
        print("Usage: incluster_test_script.py <template_name> <namespace>")
        sys.exit(1)
    run_incluster_test(sys.argv[1], sys.argv[2])
