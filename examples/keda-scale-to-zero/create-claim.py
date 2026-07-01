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

import os
import threading
import time
from datetime import datetime, timedelta, timezone
from kubernetes import client, config

# Configuration
NAMESPACE = os.getenv("NAMESPACE", "keda-test")
WARMPOOL = os.getenv("WARM_POOL_NAME", "python-sdk-warmpool")
RATE_PER_SECOND = 5
TEST_DURATION_MINUTES = 10
CLAIM_TTL_SECONDS = 60

# Initialize K8s Client
try:
    config.load_kube_config()
except config.ConfigException as e:
    if "KUBERNETES_SERVICE_HOST" in os.environ:
        config.load_incluster_config()
    else:
        raise e

custom_api = client.CustomObjectsApi()

def create_claim(index):
    """Creates a single SandboxClaim."""
    name = f"loadtest-{int(time.time())}-{index}"
    # Calculate shutdownTime as now + CLAIM_TTL_SECONDS in RFC3339 format
    shutdown_time = (datetime.now(timezone.utc) + timedelta(seconds=CLAIM_TTL_SECONDS)).strftime("%Y-%m-%dT%H:%M:%SZ")
    
    body = {
        "apiVersion": "extensions.agents.x-k8s.io/v1beta1",
        "kind": "SandboxClaim",
        "metadata": {"name": name, "namespace": NAMESPACE},
        "spec": {
            "warmPoolRef": {"name": WARMPOOL},
            "lifecycle": {
                # Delete (not Retain): at shutdownTime the controller deletes the claim AND
                # its Sandbox. Retain would free the Sandbox/Pod but leave the claim object
                # behind (status Expired), so claims would pile up over the run.
                "shutdownPolicy": "Delete",
                "shutdownTime": shutdown_time
            }
        }
    }
    try:
        custom_api.create_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1beta1",
            namespace=NAMESPACE,
            plural="sandboxclaims",
            body=body
        )
    except Exception as e:
        print(f"Error creating {name}: {e}")

if __name__ == "__main__":
    print(f"Starting load test: {RATE_PER_SECOND} claim/sec for {TEST_DURATION_MINUTES}m")

    start_time = time.time()
    end_time = start_time + (TEST_DURATION_MINUTES * 60)
    interval = 1.0 / RATE_PER_SECOND
    next_t = start_time
    counter = 0

    try:
        while time.time() < end_time:
            # Fire and forget the creation in a thread to avoid blocking the clock
            threading.Thread(target=create_claim, args=(counter,), daemon=True).start()
            counter += 1

            next_t += interval
            delay = next_t - time.time()
            if delay > 0:
                time.sleep(delay)
            else:
                next_t = time.time()  # fell behind; don't burst to catch up

            if counter % 10 == 0:
                print(f"Progress: {counter} claims created...")

    except KeyboardInterrupt:
        print("Test stopped by user.")

    print(f"Load test complete. Total claims created: {counter}")
    # The last claims expire CLAIM_TTL_SECONDS after they were created; wait that long so
    # shutdownPolicy=Delete can clean up the final batch (claims + sandboxes) before exit.
    print(f"Waiting {CLAIM_TTL_SECONDS}s for the final claims to expire and self-delete...")
    time.sleep(CLAIM_TTL_SECONDS)
