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
cd ../../../../
#pip install pyyaml
make build
make deploy-kind EXTENSIONS=true 
cd examples/python-runtime-sandbox

echo "Building sandbox-runtime image..."
docker build -t sandbox-runtime .

echo "Loading sandbox-runtime image into kind cluster..."
kind load docker-image sandbox-runtime:latest --name "${KIND_CLUSTER_NAME}"

cd ../../clients/python/agentic-sandbox-client
echo "Applying CRD for template - Sandbox claim will be applied by the sandbox client in python code"
kubectl apply -f python-sandbox-template.yaml


cd sandbox-router
echo "Building sandbox-router image..."
docker build -t sandbox-router .

echo "Loading sandbox-router image into kind cluster..."
kind load docker-image sandbox-router:latest --name "${KIND_CLUSTER_NAME}"

echo "Applying CRD for router template"
kubectl apply -f sandbox_router.yaml
sleep 60  # wait for sandbox-router to be ready

# echo "Setting up cloud-provider-kind for gateway ..."
# go install sigs.k8s.io/cloud-provider-kind@latest
# sudo install ~/go/bin/cloud-provider-kind /usr/local/bin

# echo "Starting cloud-provider-kind and enabling the Gateway API controller ..."
# cloud-provider-kind --gateway-channel standard &
# echo "Cloud Provider started."
# sleep 60

cd ../gateway-kind

echo "Setting up Cloud Provider Kind Gateway in the kind cluster..."
echo "Applying Gateway configuration..."
kubectl apply -f gateway-kind.yaml
sleep 60  # wait for the gateway to be ready

cd ../


# Cleanup function
cleanup() {
    echo "Cleaning up python-runtime and sandbox controller..."
    killall cloud-provider-kind
    kubectl delete --ignore-not-found -f python-sandbox-template.yaml
    kubectl delete --ignore-not-found statefulset agent-sandbox-controller -n agent-sandbox-system
    kubectl delete --ignore-not-found crd sandboxes.agents.x-k8s.io
    echo "Deleting kind cluster..."
    cd ../../../../
    make delete-kind || true
    echo "Cleanup completed."
}
trap cleanup EXIT


echo "========= $0 - Running the Python client tester... ========="
python3 ./test_client.py --gateway-name kind-gateway
echo "========= $0 - Finished running the Python client with gateway and router tester. ========="


trap cleanup EXIT

echo "Test finished."
# The 'trap' command will now execute the cleanup function.
