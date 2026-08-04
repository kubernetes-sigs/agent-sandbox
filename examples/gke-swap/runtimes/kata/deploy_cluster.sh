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

CLUSTER_NAME=${CLUSTER_NAME:-"agent-sandbox-kata"}
ZONE=${ZONE:-"us-east1-b"}
KATA_VERSION=${KATA_VERSION:-"3.2.0"}

# Get the directory of this script
DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
REPO_ROOT="$(cd "${DIR}/../../../.." && pwd)"
export KUBECONFIG="${KUBECONFIG:-"${REPO_ROOT}/bin/KUBECONFIG"}"

echo "Creating GKE cluster base (with small default pool, Ubuntu)..."
gcloud container clusters create "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --num-nodes 1 \
    --machine-type e2-standard-4 \
    --image-type ubuntu_containerd \
    --enable-ip-alias

echo "Creating baseline node pool (no swap, Ubuntu, Nested Virtualization enabled)..."
gcloud container node-pools create baseline-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --machine-type c4-standard-8 \
    --num-nodes 1 \
    --max-pods-per-node 256 \
    --disk-size 250 \
    --image-type ubuntu_containerd \
    --enable-nested-virtualization \
    --node-labels agent-sandbox.dev/kata=true

echo "Creating lssd-swap node pool (with dedicated LSSD swap, Ubuntu, Nested Virtualization enabled)..."
gcloud container node-pools create lssd-swap-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --machine-type c4-standard-8-lssd \
    --num-nodes 1 \
    --max-pods-per-node 256 \
    --disk-size 250 \
    --image-type ubuntu_containerd \
    --enable-nested-virtualization \
    --node-labels agent-sandbox.dev/kata=true \
    --system-config-from-file "${DIR}/../../swap-dedicated-lssd.yaml"

echo "Fetching cluster credentials..."
mkdir -p "${REPO_ROOT}/bin"
gcloud container clusters get-credentials "${CLUSTER_NAME}" --zone "${ZONE}"

echo "Installing Kata Containers via kata-deploy..."
KATA_RBAC_URL="https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_VERSION}/tools/packaging/kata-deploy/kata-rbac/base/kata-rbac.yaml"
kubectl apply -f "${KATA_RBAC_URL}"

KATA_DEPLOY_URL="https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_VERSION}/tools/packaging/kata-deploy/kata-deploy/base/kata-deploy.yaml"
kubectl apply -f "${KATA_DEPLOY_URL}"

echo "Waiting for Kata installation to complete..."
kubectl -n kube-system rollout status daemonset/kata-deploy --timeout=10m

echo "Registering RuntimeClasses 'kata-qemu' and 'kata-clh'..."
cat <<EOF | kubectl apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-qemu
handler: kata-qemu
scheduling:
  nodeSelector:
    kubernetes.io/os: linux
    agent-sandbox.dev/kata: "true"
EOF

cat <<EOF | kubectl apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-clh
handler: kata-clh
scheduling:
  nodeSelector:
    kubernetes.io/os: linux
    agent-sandbox.dev/kata: "true"
EOF

echo "Cluster deployed successfully."
echo "Please ensure the Agent Sandbox controller and CRDs (including extensions) are deployed on this cluster before running the tests."
# Example installation (all-in-one controller + extension CRDs):
# kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox-with-extensions.yaml
