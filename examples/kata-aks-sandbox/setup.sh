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


# This script creates billable Azure resources: a resource group and an AKS
# cluster with Pod Sandboxing (Kata) enabled, or -- with --reuse-cluster -- a
# new Kata node pool on an existing cluster. It then fetches credentials and
# verifies the AKS-provided kata-vm-isolation RuntimeClass.

# Exit immediately if a command exits with a non-zero status.
set -e

# Defaults
RESOURCE_GROUP="kata-aks-demo"
CLUSTER_NAME="kata-test"
LOCATION="westus2"
NODE_COUNT=1
VM_SIZE="Standard_D4s_v5"
KATA_NODEPOOL_NAME="katapool"
RUNTIME_CLASS_NAME="kata-vm-isolation"
REUSE_CLUSTER=false

# Parse arguments
while [[ "$#" -gt 0 ]]; do
    case $1 in
        --resource-group) RESOURCE_GROUP="$2"; shift ;;
        --cluster-name) CLUSTER_NAME="$2"; shift ;;
        --location) LOCATION="$2"; shift ;;
        --node-count) NODE_COUNT="$2"; shift ;;
        --vm-size) VM_SIZE="$2"; shift ;;
        --kata-nodepool-name) KATA_NODEPOOL_NAME="$2"; shift ;;
        --reuse-cluster) REUSE_CLUSTER=true ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

echo "### Configuration ###"
echo "RESOURCE_GROUP:      ${RESOURCE_GROUP}"
echo "CLUSTER_NAME:        ${CLUSTER_NAME}"
echo "LOCATION:            ${LOCATION}"
echo "NODE_COUNT:          ${NODE_COUNT}"
echo "VM_SIZE:             ${VM_SIZE}"
echo "KATA_NODEPOOL_NAME:  ${KATA_NODEPOOL_NAME}"
echo "REUSE_CLUSTER:       ${REUSE_CLUSTER}"
echo "#####################"

echo "### Step 1: Creating/Checking the Resource Group ###"
if ! az group show --name "${RESOURCE_GROUP}" > /dev/null 2>&1; then
    echo "--- Creating resource group '${RESOURCE_GROUP}' in '${LOCATION}' ---"
    az group create --name "${RESOURCE_GROUP}" --location "${LOCATION}" --output none
fi

echo "### Step 2: Creating/Checking the AKS Cluster ###"

if az aks show --name "${CLUSTER_NAME}" --resource-group "${RESOURCE_GROUP}" > /dev/null 2>&1; then
    if [[ "${REUSE_CLUSTER}" != "true" ]]; then
        echo "Error: Cluster '${CLUSTER_NAME}' already exists in resource group '${RESOURCE_GROUP}'."
        echo "To add a Kata node pool to the existing cluster, re-run with the --reuse-cluster flag."
        exit 1
    fi
    echo "### Cluster '${CLUSTER_NAME}' exists. Adding a Kata node pool (requires Kubernetes >= 1.27) ###"
    # AKS provisions Kata (runtime, guest kernel, Cloud Hypervisor) on the pool
    # and labels its nodes for the kata-vm-isolation RuntimeClass scheduling.
    az aks nodepool add \
        --cluster-name "${CLUSTER_NAME}" \
        --resource-group "${RESOURCE_GROUP}" \
        --name "${KATA_NODEPOOL_NAME}" \
        --node-count "${NODE_COUNT}" \
        --os-sku AzureLinux \
        --workload-runtime KataVmIsolation \
        --node-vm-size "${VM_SIZE}"
else
    echo "### Cluster '${CLUSTER_NAME}' not found. Creating it with Pod Sandboxing enabled ###"
    az aks create \
        --name "${CLUSTER_NAME}" \
        --resource-group "${RESOURCE_GROUP}" \
        --os-sku AzureLinux \
        --workload-runtime KataVmIsolation \
        --node-vm-size "${VM_SIZE}" \
        --node-count "${NODE_COUNT}" \
        --generate-ssh-keys
fi

echo "### Step 3: Getting Cluster Credentials ###"
az aks get-credentials --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" --overwrite-existing

echo "### Step 4: Verifying the Kata RuntimeClass ###"
# AKS ships this RuntimeClass on Pod Sandboxing clusters; nothing to install.
kubectl get runtimeclass "${RUNTIME_CLASS_NAME}"

echo "### Setup Complete! ###"
echo "You can now deploy an Agent Sandbox using the '${RUNTIME_CLASS_NAME}' RuntimeClass."
echo "Follow 'Step 2' in the README to install the Agent Sandbox controller and 'Step 3' to deploy the sandbox."
