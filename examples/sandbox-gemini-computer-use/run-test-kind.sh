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

if [ ! -d "computer-use-preview" ]; then
    git clone https://github.com/google-gemini/computer-use-preview
fi

if [ "$NO_BUILD" = false ]; then
    # following develop guide to make and deploy agent-sandbox to kind cluster
    cd ../../
    #pip install pyyaml
    echo "Building and deploying agent-sandbox to kind cluster..."
    make build
    make deploy-kind EXTENSIONS=true
    cd examples/sandbox-gemini-computer-use
    echo "Building sandbox-gemini-runtime image..."
    docker build -t sandbox-gemini-runtime  .
fi

# Cleanup function
cleanup() {
    echo "Cleaning up..."
    kubectl delete --ignore-not-found -f service.yaml
    kubectl delete --ignore-not-found sandboxclaim sandbox-computeruse-claim
    kubectl delete --ignore-not-found secret gemini-api-key
    kubectl delete --ignore-not-found -f sandbox-gemini-computer-use.yaml
    
}
trap cleanup EXIT

echo "Loading sandbox-runtime image into kind cluster..."
kind load docker-image sandbox-gemini-runtime:latest --name "${KIND_CLUSTER_NAME}"

echo "Applying CRD and deployment..."
kubectl apply -f sandbox-gemini-computer-use.yaml

echo "Running the programmatic test..."
python3 -m agentic-sandbox-client.test_computeruse


echo "Test finished."
