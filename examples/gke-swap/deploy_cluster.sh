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

set -eo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-"agent-sandbox-swap"}
REGION=${REGION:-"us-east4"}
ZONE=${ZONE:-"us-east4-a"}
BASELINE_MACHINE_TYPE=${BASELINE_MACHINE_TYPE:-"c4-standard-8"}
SWAP_MACHINE_TYPE=${SWAP_MACHINE_TYPE:-"c4-standard-8-lssd"}

# Get the directory of this script
DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

echo "Creating GKE cluster base (with small default pool)..."
gcloud container clusters create "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --num-nodes 1 \
    --machine-type e2-standard-4

echo "Creating baseline node pool (no swap)..."
gcloud container node-pools create baseline-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --machine-type "${BASELINE_MACHINE_TYPE}" \
    --num-nodes 1 \
    --max-pods-per-node 256
 
echo "Creating lssd-swap node pool (with dedicated LSSD swap)..."
# We use c4-standard-8-lssd which comes with 1 local SSD.
# We dedicate this single SSD entirely to swap (via swap-dedicated-lssd.yaml).
gcloud container node-pools create lssd-swap-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --machine-type "${SWAP_MACHINE_TYPE}" \
    --num-nodes 1 \
    --max-pods-per-node 256 \
    --system-config-from-file "${DIR}/swap-dedicated-lssd.yaml"

echo "Fetching cluster credentials..."
REPO_ROOT="${DIR}/../.."
mkdir -p "${REPO_ROOT}/bin"
KUBECONFIG="${KUBECONFIG:-"${REPO_ROOT}/bin/KUBECONFIG"}" gcloud container clusters get-credentials "${CLUSTER_NAME}" --zone "${ZONE}"

echo "Cluster deployed successfully."
echo "Please ensure the Agent Sandbox controller and CRDs are deployed on this cluster before running the tests."
# Example installation:
# kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox.yaml
