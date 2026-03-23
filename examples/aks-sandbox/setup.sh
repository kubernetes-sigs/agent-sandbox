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

# This script creates an AKS cluster suitable for running Agent Sandbox.
# It optionally creates a spot node pool for cost-effective sandbox workloads.
#
# Usage: ./setup.sh [OPTIONS...]
#
# Run ./setup.sh --help for available options.

set -e

# Defaults
RESOURCE_GROUP="agent-sandbox-rg"
LOCATION="eastus"
CLUSTER_NAME="agent-sandbox-cluster"
NODE_COUNT=1
VM_SIZE="Standard_D4s_v3"
ENABLE_SPOT=false
SPOT_NODE_COUNT=2
SPOT_VM_SIZE="Standard_D4s_v3"

usage() {
    echo "Usage: $0 [OPTIONS...]"
    echo ""
    echo "Creates an AKS cluster for Agent Sandbox."
    echo ""
    echo "Options:"
    echo "  --resource-group NAME   Azure resource group (default: ${RESOURCE_GROUP})"
    echo "  --location LOCATION     Azure region (default: ${LOCATION})"
    echo "  --cluster-name NAME     AKS cluster name (default: ${CLUSTER_NAME})"
    echo "  --node-count N          System node pool size (default: ${NODE_COUNT})"
    echo "  --vm-size SIZE          VM size for system pool (default: ${VM_SIZE})"
    echo "  --enable-spot           Create a spot node pool for sandboxes"
    echo "  --spot-node-count N     Spot node pool size (default: ${SPOT_NODE_COUNT})"
    echo "  --spot-vm-size SIZE     VM size for spot pool (default: ${SPOT_VM_SIZE})"
    echo "  --help                  Show this help message"
    exit 0
}

# Parse arguments
while [[ "$#" -gt 0 ]]; do
    case $1 in
        --resource-group) RESOURCE_GROUP="$2"; shift ;;
        --location) LOCATION="$2"; shift ;;
        --cluster-name) CLUSTER_NAME="$2"; shift ;;
        --node-count) NODE_COUNT="$2"; shift ;;
        --vm-size) VM_SIZE="$2"; shift ;;
        --enable-spot) ENABLE_SPOT=true ;;
        --spot-node-count) SPOT_NODE_COUNT="$2"; shift ;;
        --spot-vm-size) SPOT_VM_SIZE="$2"; shift ;;
        --help) usage ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

echo "### Configuration ###"
echo "RESOURCE_GROUP:   ${RESOURCE_GROUP}"
echo "LOCATION:         ${LOCATION}"
echo "CLUSTER_NAME:     ${CLUSTER_NAME}"
echo "NODE_COUNT:       ${NODE_COUNT}"
echo "VM_SIZE:          ${VM_SIZE}"
echo "ENABLE_SPOT:      ${ENABLE_SPOT}"
if [[ "${ENABLE_SPOT}" == "true" ]]; then
    echo "SPOT_NODE_COUNT:  ${SPOT_NODE_COUNT}"
    echo "SPOT_VM_SIZE:     ${SPOT_VM_SIZE}"
fi
echo "#####################"

echo ""
echo "### Step 1: Creating Resource Group ###"

az group create \
    --name "${RESOURCE_GROUP}" \
    --location "${LOCATION}" \
    --output table

echo "### Step 2: Creating AKS Cluster ###"

if az aks show --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" > /dev/null 2>&1; then
    echo "### Cluster '${CLUSTER_NAME}' already exists. Skipping creation. ###"
else
    echo "### Creating AKS cluster '${CLUSTER_NAME}'... ###"
    if ! az aks create \
        --resource-group "${RESOURCE_GROUP}" \
        --name "${CLUSTER_NAME}" \
        --location "${LOCATION}" \
        --node-count "${NODE_COUNT}" \
        --node-vm-size "${VM_SIZE}" \
        --generate-ssh-keys \
        --output table; then
        echo "### AKS cluster creation failed. ###"
        exit 1
    fi
    echo "### AKS cluster '${CLUSTER_NAME}' created successfully. ###"
fi

echo "### Step 3: Getting Cluster Credentials ###"

az aks get-credentials \
    --resource-group "${RESOURCE_GROUP}" \
    --name "${CLUSTER_NAME}" \
    --overwrite-existing

if [[ "${ENABLE_SPOT}" == "true" ]]; then
    echo ""
    echo "### Step 4: Adding Spot Node Pool ###"

    if az aks nodepool show --resource-group "${RESOURCE_GROUP}" --cluster-name "${CLUSTER_NAME}" --name sandboxpool > /dev/null 2>&1; then
        echo "### Spot node pool 'sandboxpool' already exists. Skipping. ###"
    else
        echo "### Creating spot node pool 'sandboxpool'... ###"
        if ! az aks nodepool add \
            --resource-group "${RESOURCE_GROUP}" \
            --cluster-name "${CLUSTER_NAME}" \
            --name sandboxpool \
            --node-count "${SPOT_NODE_COUNT}" \
            --node-vm-size "${SPOT_VM_SIZE}" \
            --priority Spot \
            --eviction-policy Delete \
            --spot-max-price -1 \
            --labels sandbox=true \
            --node-taints "kubernetes.azure.com/scalesetpriority=spot:NoSchedule" \
            --output table; then
            echo "### Spot node pool creation failed. ###"
            exit 1
        fi
        echo "### Spot node pool 'sandboxpool' created successfully. ###"
    fi
fi

echo ""
echo "### Setup Complete! ###"
echo "Next steps:"
echo "  1. Install the Agent Sandbox controller (see README.md Step 2)"
echo "  2. Deploy a sandbox: kubectl apply -f sandbox.yaml"
