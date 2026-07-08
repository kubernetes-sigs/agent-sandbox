#!/bin/bash
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

set -e

export KIND_CLUSTER_NAME="agent-sandbox"

# Resolve paths from the script location so it works from any cwd
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

cd "${REPO_ROOT}"
# Only build/deploy if user asks or if we want to ensure latest controller
# make build
# make deploy-kind
cd "${SCRIPT_DIR}"

echo "Pulling ghcr.io/nullclaw/nullclaw:v2026.5.29..."
docker pull ghcr.io/nullclaw/nullclaw:v2026.5.29

echo "Loading ghcr.io/nullclaw/nullclaw:v2026.5.29 into kind cluster..."
kind load docker-image ghcr.io/nullclaw/nullclaw:v2026.5.29 --name "${KIND_CLUSTER_NAME}"

echo "Applying config and sandbox resources..."
kubectl apply -f nullclaw-config.yaml
kubectl apply -f nullclaw-sandbox.yaml

# Cleanup function
cleanup() {
    echo "Cleaning up..."
    kubectl delete --ignore-not-found -f nullclaw-sandbox.yaml
    kubectl delete --ignore-not-found -f nullclaw-config.yaml
}
# trap cleanup EXIT

echo "Waiting for sandbox pod to be ready..."
kubectl wait --for=condition=ready pod --selector=sandbox=nullclaw-sandbox --timeout=120s

echo "Port-forwarding gateway..."
POD_NAME=$(kubectl get pods -l sandbox=nullclaw-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "pod/${POD_NAME}" 3000:3000 &
PF_PID=$!

trap "kill $PF_PID" EXIT

sleep 5

echo "Checking Gateway health..."
if ! curl -sf http://127.0.0.1:3000/health; then
    echo ""
    echo "Gateway health check failed" >&2
    exit 1
fi
echo ""

echo "Test finished."
