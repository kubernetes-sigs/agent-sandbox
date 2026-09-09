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

set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cd "${SCRIPT_DIR}"

echo "Building pi-sandbox:local..."
docker build -t pi-sandbox:local .

echo "Loading pi-sandbox:local into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image pi-sandbox:local --name "${KIND_CLUSTER_NAME}"

echo "Applying sandbox manifest..."
kubectl apply -f sandbox.yaml

echo "Waiting for sandbox pod to be ready..."
kubectl wait --for=condition=ready pod -l sandbox=pi-code-agent --timeout=120s

POD_NAME="$(kubectl get pods -l sandbox=pi-code-agent -o jsonpath='{.items[0].metadata.name}')"

echo ""
echo "Pi sandbox is running (pod: ${POD_NAME})."
echo "Attach with:"
echo "  kubectl attach -it ${POD_NAME}"
echo "Detach without killing Pi: Ctrl+P, then Ctrl+Q"
