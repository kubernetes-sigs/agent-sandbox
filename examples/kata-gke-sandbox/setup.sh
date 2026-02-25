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


# Exit immediately if a command exits with a non-zero status.
set -e

# Defaults
CLUSTER_NAME="kata-test"
ZONE="us-south1-a"
NUM_NODES=2
MACHINE_TYPE="n2-standard-4"
IMAGE_TYPE="UBUNTU_CONTAINERD"
KATA_VERSION="3.2.0"
RUNTIME_CLASS_NAME="kata-qemu"
RUNTIME_CLASS_FILE="kata-runtime.yaml"
REUSE_CLUSTER=false

# Parse arguments
while [[ "$#" -gt 0 ]]; do
    case $1 in
        --cluster-name) CLUSTER_NAME="$2"; shift ;;
        --zone) ZONE="$2"; shift ;;
        --num-nodes) NUM_NODES="$2"; shift ;;
        --machine-type) MACHINE_TYPE="$2"; shift ;;
        --image-type) IMAGE_TYPE="$2"; shift ;;
        --kata-version) KATA_VERSION="$2"; shift ;;
        --runtime-class-name) RUNTIME_CLASS_NAME="$2"; shift ;;
        --runtime-class-file) RUNTIME_CLASS_FILE="$2"; shift ;;
        --reuse-cluster) REUSE_CLUSTER=true ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

echo "### Configuration ###"
echo "CLUSTER_NAME:        ${CLUSTER_NAME}"
echo "ZONE:                ${ZONE}"
echo "NUM_NODES:           ${NUM_NODES}"
echo "MACHINE_TYPE:        ${MACHINE_TYPE}"
echo "IMAGE_TYPE:          ${IMAGE_TYPE}"
echo "KATA_VERSION:        ${KATA_VERSION}"
echo "RUNTIME_CLASS_NAME:  ${RUNTIME_CLASS_NAME}"
echo "RUNTIME_CLASS_FILE:  ${RUNTIME_CLASS_FILE}"
echo "REUSE_CLUSTER:       ${REUSE_CLUSTER}"
echo "#####################"

echo "### Step 1: Creating/Checking GKE Cluster ###"

# Check if the cluster already exists
if gcloud container clusters describe "${CLUSTER_NAME}" --zone "${ZONE}" > /dev/null 2>&1; then
    # Exit code 0 means the cluster exists
    if [[ "${REUSE_CLUSTER}" == "true" ]]; then
        echo "### Cluster '${CLUSTER_NAME}' already exists in zone '${ZONE}'. Reusing as --reuse-cluster is set. ###"
    else
        echo "Error: Cluster '${CLUSTER_NAME}' already exists in zone '${ZONE}'."
        echo "To force setup on the existing cluster, please run the script with the --reuse-cluster flag."
        exit 1
    fi
else
    # Exit code non-zero means the cluster does not exist, so create it
    echo "### Cluster '${CLUSTER_NAME}' not found. Creating new cluster... ###"
    if ! gcloud container clusters create "${CLUSTER_NAME}" \
        --zone "${ZONE}" \
        --num-nodes "${NUM_NODES}" \
        --machine-type "${MACHINE_TYPE}" \
        --image-type "${IMAGE_TYPE}" \
        --enable-nested-virtualization; then
        echo "### GKE Cluster creation failed. ###"
        exit 1
    else
        echo "### GKE Cluster '${CLUSTER_NAME}' created successfully. ###"
    fi
fi

echo "### Step 2: Installing the Kata Infrastructure ###"

echo "--- Getting cluster credentials ---"
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --location="${ZONE}"

echo "--- Applying RBAC permissions ---"
KATA_RBAC_URL="https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_VERSION}/tools/packaging/kata-deploy/kata-rbac/base/kata-rbac.yaml"
echo "Using RBAC URL: ${KATA_RBAC_URL}"
kubectl apply -f "${KATA_RBAC_URL}"

echo "--- Applying the Installer DaemonSet ---"
KATA_DEPLOY_URL="https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_VERSION}/tools/packaging/kata-deploy/kata-deploy/base/kata-deploy.yaml"
echo "Using Deploy URL: ${KATA_DEPLOY_URL}"
kubectl apply -f "${KATA_DEPLOY_URL}"

echo "--- Waiting for Kata installation to complete ---"
echo "This might take a few minutes..."
# Wait for the kata-deploy daemonset to be fully rolled out
kubectl -n kube-system rollout status daemonset/kata-deploy --timeout=10m
echo "--- Kata installation complete ---"

echo "### Step 3: Registering the RuntimeClass ###"

echo "--- Creating RuntimeClass manifest file: ${RUNTIME_CLASS_FILE} ---"
cat <<EOF > "${RUNTIME_CLASS_FILE}"
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: ${RUNTIME_CLASS_NAME}
handler: ${RUNTIME_CLASS_NAME}
scheduling:
  nodeSelector:
    kubernetes.io/os: linux
EOF

echo "--- Applying RuntimeClass manifest ---"
kubectl apply -f "${RUNTIME_CLASS_FILE}"

echo "--- RuntimeClass '${RUNTIME_CLASS_NAME}' created ---"

echo "### Setup Complete! ###"
echo "You can now deploy an Agent Sandbox using the '${RUNTIME_CLASS_NAME}' RuntimeClass."
echo "Follow 'Step 2' in the guide to deploy the sandbox and 'Step 3' to verify the isolation."

