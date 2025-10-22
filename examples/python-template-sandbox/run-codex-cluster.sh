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

# --- Configuration ---
PROCESS_ICON="⚙️"
DONE_ICON="✅"
export KIND_CLUSTER_NAME="agent-sandbox"

# --- Helper Functions ---
info() {
    echo "${PROCESS_ICON} $1"
}

success() {
    echo "${DONE_ICON} $1"
}

cleanup() {
    info "--- Cleaning up ---"
    info "Deleting Kubernetes resources..."
    kubectl delete --ignore-not-found -f sandbox-python-claim.yaml
    kubectl delete --ignore-not-found -f sandbox-python-template.yaml
    
    info "Deleting agent-sandbox-controller..."
    kubectl delete --ignore-not-found statefulset agent-sandbox-controller -n agent-sandbox-system
    kubectl delete --ignore-not-found crd sandboxes.agents.x-k8s.io
    
    info "Deleting Kind cluster..."
    cd ../../
    make delete-kind || true
    cd examples/python-template-sandbox
    success "Cleanup complete."
}

# --- Main Script ---
trap cleanup EXIT

info "--- Setting up Python Runtime Cluster ---"

# 1. Build and Deploy Agent Sandbox
info "Navigating to the root directory..."
cd ../../

info "Building the agent-sandbox..."
make build

info "Deploying agent-sandbox to Kind cluster..."
make deploy-kind EXTENSIONS=true

info "Navigating back to the python-template-sandbox directory..."
cd examples/python-template-sandbox

# 2. Build and Load Docker Image
info "Building the sandbox-runtime Docker image..."
docker build -t sandbox-runtime .

info "Loading the sandbox-runtime image into the Kind cluster..."
kind load docker-image sandbox-runtime:latest --name "${KIND_CLUSTER_NAME}"

# 3. Apply CRDs (if needed)
info "Applying CRD for template. The Sandbox claim will be applied by the sandbox client in the Python code."
kubectl apply -f sandbox-python-template.yaml
#kubectl apply -f sandbox-python-claim.yaml

# 4. Install Python Client
info "Installing the Python client..."
pip install -e ./agentic-sandbox-client

# 5. Pause for User
read -p "${DONE_ICON} ${KIND_CLUSTER_NAME} Cluster is running. Press Enter to shutdown and cleanup..."

# The 'trap' command will now execute the cleanup function.