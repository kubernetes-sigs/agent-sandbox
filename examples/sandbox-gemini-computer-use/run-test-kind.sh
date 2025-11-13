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
    echo "Building agent-sandbox controller"
    make build
    #make deploy-kind EXTENSIONS=true
    cd examples/sandbox-gemini-computer-use
    echo "Building sandbox-gemini-runtime image..."
    docker build -t sandbox-gemini-runtime  .
fi

echo "Deploying kind cluster with agent-sandbox controller..."
cd ../../
make deploy-kind EXTENSIONS=true
cd examples/sandbox-gemini-computer-use

# Cleanup function
cleanup() {
    echo "Cleaning up template, secrets, controller..."
    kubectl delete --ignore-not-found secret gemini-api-key
    kubectl delete --ignore-not-found -f sandbox-gemini-computer-use.yaml
    kubectl delete --ignore-not-found statefulset agent-sandbox-controller -n agent-sandbox-system
    kubectl delete --ignore-not-found crd sandboxes.agents.x-k8s.io
    echo "Deleting kind cluster..."
    cd ../../
    make delete-kind || true
    cd examples/sandbox-gemini-computer-use
}


echo "Loading sandbox-runtime image into kind cluster..."
kind load docker-image sandbox-gemini-runtime:latest --name "${KIND_CLUSTER_NAME}"

echo "Applying CRD template for gemini computer use..."
kubectl apply -f sandbox-gemini-computer-use.yaml

echo "========= $0 - Running the Python client tester... ========="
python3 -m agentic-sandbox-client.test_computeruse
echo "========= $0 - Finished running the Python client tester. ========="

trap cleanup EXIT
