#!/bin/bash
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

set -e

export KIND_CLUSTER_NAME="agent-sandbox"
NO_BUILD=false
INTERACTIVE_FLAG=""
SANDBOX_ROUTER_IMG="sandbox-router:latest"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --nobuild)
            NO_BUILD=true
            shift
            ;;
        --interactive)
            INTERACTIVE_FLAG="true"
            shift
            ;;
        *)
            echo "Unknown parameter passed: $1"
            exit 1
            ;;
    esac
done

if [ "$NO_BUILD" = false ]; then
    # following develop guide to make and deploy agent-sandbox to kind cluster
    SCRIPT_DIR=$(dirname "$0")
    PROJECT_ROOT=$(readlink -f "$SCRIPT_DIR/../../")

    echo "Building and deploying agent-sandbox to kind cluster from: $PROJECT_ROOT"
    (cd "$PROJECT_ROOT" && make build)
    (cd "$PROJECT_ROOT" && make deploy-kind EXTENSIONS=true)
    
    echo "Building sandbox-gemini-runtime image..."
    docker build -t sandbox-gemini-runtime:latest --load "$SCRIPT_DIR" # Build from the script's directory and load it
    (cd "$PROJECT_ROOT/clients/python/agentic-sandbox-client/sandbox-router" && docker buildx build --load -t "${SANDBOX_ROUTER_IMG}" .)

    echo "Loading sandbox-router image into kind cluster..."
    kind load docker-image "${SANDBOX_ROUTER_IMG}" --name "${KIND_CLUSTER_NAME}"
    echo "Loading sandbox-runtime image into kind cluster..."
    kind load docker-image sandbox-gemini-runtime:latest --name "${KIND_CLUSTER_NAME}"
fi

SCRIPT_DIR=$(dirname "$0")
PROJECT_ROOT=$(readlink -f "$SCRIPT_DIR/../../")
echo "DEBUG: SCRIPT_DIR is $SCRIPT_DIR"
echo "DEBUG: PROJECT_ROOT is $PROJECT_ROOT"

# Create a temporary kubeconfig file for kind
KUBECFG_FILE=$(mktemp)
kind get kubeconfig --name "${KIND_CLUSTER_NAME}" > "$KUBECFG_FILE"
export KUBECONFIG="$KUBECFG_FILE"

# Cleanup function
cleanup() {
    echo "Cleaning up..."
    kubectl delete --ignore-not-found -f "$PROJECT_ROOT/clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml"
    kubectl delete --ignore-not-found sandboxclaim sandbox-computeruse-claim
    kubectl delete --ignore-not-found secret gemini-api-key
    kubectl delete --ignore-not-found -f "$SCRIPT_DIR/sandbox-gemini-computer-use.yaml"
    rm -f "$KUBECFG_FILE" # Delete the temporary kubeconfig file
}
trap cleanup EXIT

echo "Applying CRD and deployment..."
(cd "$PROJECT_ROOT/clients/python/agentic-sandbox-client/sandbox-router" && kubectl apply -f sandbox_router.yaml)
kubectl apply -f "$SCRIPT_DIR/sandbox-gemini-computer-use.yaml"

# Ensure the local client is up-to-date for running tests
(cd "$PROJECT_ROOT" && pip install -e . --break-system-packages)

echo "Running the programmatic test..."
(cd "$PROJECT_ROOT" && python3 -m unittest "clients.python.agentic-sandbox-client.test_computer_use_extension")

echo "Test finished."
