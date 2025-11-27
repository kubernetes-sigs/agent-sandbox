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

# following develop guide to make and deploy agent-sandbox to kind cluster
cd ../../
make build 
make deploy-kind
cd examples/python-runtime-sandbox

echo "Building sandbox-runtime image..."
docker build -t sandbox-runtime .

echo "Loading sandbox-runtime image into kind cluster..."
kind load docker-image sandbox-runtime:latest --name "${KIND_CLUSTER_NAME}"


echo "Applying CRD and deployment..."
kubectl apply -f sandbox-python-kind.yaml

# Cleanup function
cleanup() {
    echo "Cleaning up python-runtime and sandbox controller..."
    kubectl delete --timeout=10s --ignore-not-found -f sandbox-python-kind.yaml
    kubectl delete --timeout=10s --ignore-not-found deployment agent-sandbox-controller -n agent-sandbox-system
    kubectl delete --timeout=10s --ignore-not-found crd sandboxes.agents.x-k8s.io
    echo "Deleting kind cluster..." 
    cd ../../
    make delete-kind
    cd examples/python-runtime-sandbox
}
trap cleanup EXIT

echo "Waiting for sandbox pod to be ready..."
kubectl wait --for=condition=ready pod --selector=sandbox=my-python-sandbox --timeout=60s

echo "Port-forwarding service..."
POD_NAME=$(kubectl get pods -l sandbox=my-python-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "pod/${POD_NAME}" 8888:8888 &
PF_PID=$!

# Additional cleanup for port-forward
trap "kill $PF_PID; cleanup" EXIT

# Give port-forward a moment to establish
sleep 3

echo "Running the Python tester..."
python3 tester.py 127.0.0.1 8888

echo "Test finished."
# The 'trap' command will now execute the cleanup function.