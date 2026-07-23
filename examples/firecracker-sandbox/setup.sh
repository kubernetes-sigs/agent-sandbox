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

# Setup script for Kata Containers with Firecracker (kata-fc) on a Linux node.
# Installs kata-deploy, registers the kata-fc RuntimeClass, and labels nodes.

set -e

# Defaults
KATA_VERSION="3.2.0"
RUNTIME_CLASS_NAME="kata-fc"
NODE_LABEL_KEY="kata-firecracker"
NODE_LABEL_VALUE="true"
LABEL_NODES=true
INSTALL_KATA=true
CHECK_KVM=true

# Parse arguments
while [[ "$#" -gt 0 ]]; do
    case $1 in
        --kata-version)       KATA_VERSION="$2"; shift ;;
        --runtime-class-name)  RUNTIME_CLASS_NAME="$2"; shift ;;
        --node-label-key)      NODE_LABEL_KEY="$2"; shift ;;
        --node-label-value)    NODE_LABEL_VALUE="$2"; shift ;;
        --no-label)            LABEL_NODES=false ;;
        --no-install)          INSTALL_KATA=false ;;
        --skip-kvm-check)      CHECK_KVM=false ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

# Validate KATA_VERSION format to prevent path traversal
if [[ ! "$KATA_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
    echo "Error: Invalid Kata version format: '${KATA_VERSION}'"
    echo "Kata version must follow semantic versioning (e.g., 3.2.0 or 3.2.0-rc1)."
    exit 1
fi

echo "### Configuration ###"
echo "KATA_VERSION:        ${KATA_VERSION}"
echo "RUNTIME_CLASS_NAME:  ${RUNTIME_CLASS_NAME}"
echo "NODE_LABEL_KEY:      ${NODE_LABEL_KEY}"
echo "NODE_LABEL_VALUE:    ${NODE_LABEL_VALUE}"
echo "LABEL_NODES:         ${LABEL_NODES}"
echo "INSTALL_KATA:        ${INSTALL_KATA}"
echo "#####################"

#############################################################################
# Step 1: Verify KVM support
#############################################################################
echo "### Step 1: Verifying KVM support ###"
if [[ "${CHECK_KVM}" == "true" ]]; then
    if [[ ! -e /dev/kvm ]]; then
        echo "Error: /dev/kvm not found. Firecracker requires hardware virtualization."
        echo "Ensure your host CPU supports VT-x/AMD-V and that KVM kernel modules are loaded."
        echo "Try: sudo modprobe kvm && sudo modprobe kvm_intel (or kvm_amd)"
        echo ""
        echo "If you are running this script from a workstation (e.g. macOS) that drives"
        echo "a remote cluster where nodes do have KVM, pass --skip-kvm-check to bypass."
        exit 1
    fi

    if [[ ! -r /dev/kvm ]] || [[ ! -w /dev/kvm ]]; then
        echo "Warning: /dev/kvm exists but permissions look incorrect."
        echo "Try: sudo chmod 666 /dev/kvm"
    fi
    echo "### KVM support detected at /dev/kvm ###"
else
    echo "### Step 1: Skipped (--skip-kvm-check) ###"
    echo "Ensure every target node has /dev/kvm before scheduling kata-fc pods."
fi

#############################################################################
# Step 2: Install Kata Containers with Firecracker via kata-deploy
#############################################################################
if [[ "${INSTALL_KATA}" == "true" ]]; then
    echo "### Step 2: Installing Kata Containers (kata-deploy) ###"

    echo "--- Applying Kata RBAC ---"
    KATA_RBAC_URL="https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_VERSION}/tools/packaging/kata-deploy/kata-rbac/base/kata-rbac.yaml"
    echo "Using RBAC URL: ${KATA_RBAC_URL}"
    kubectl apply -f "${KATA_RBAC_URL}"

    echo "--- Applying kata-deploy DaemonSet ---"
    KATA_DEPLOY_URL="https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_VERSION}/tools/packaging/kata-deploy/kata-deploy/base/kata-deploy.yaml"
    echo "Using Deploy URL: ${KATA_DEPLOY_URL}"
    kubectl apply -f "${KATA_DEPLOY_URL}"

    echo "--- Waiting for kata-deploy rollout (this may take several minutes) ---"
    kubectl -n kube-system rollout status daemonset/kata-deploy --timeout=10m
    echo "### Kata Containers installed ###"
else
    echo "### Step 2: Skipped (--no-install) ###"
fi

#############################################################################
# Step 3: Register the kata-fc RuntimeClass
#############################################################################
echo "### Step 3: Registering RuntimeClass '${RUNTIME_CLASS_NAME}' ###"

cat <<EOF | kubectl apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: ${RUNTIME_CLASS_NAME}
handler: ${RUNTIME_CLASS_NAME}
scheduling:
  nodeSelector:
    kubernetes.io/os: linux
    ${NODE_LABEL_KEY}: "${NODE_LABEL_VALUE}"
EOF

echo "### RuntimeClass '${RUNTIME_CLASS_NAME}' created ###"

#############################################################################
# Step 4: Label nodes
#############################################################################
if [[ "${LABEL_NODES}" == "true" ]]; then
    echo "### Step 4: Labeling all Linux nodes with ${NODE_LABEL_KEY}=${NODE_LABEL_VALUE} ###"
    echo "Note: KVM was only verified on the host running this script."
    echo "      Ensure every Linux node in the cluster actually has /dev/kvm"
    echo "      before scheduling kata-fc pods onto them."
    echo "      Use --no-label to skip this step and label nodes manually."
    NODES=$(kubectl get nodes -l kubernetes.io/os=linux -o name)
    if [[ -z "${NODES}" ]]; then
        echo "Warning: no Linux nodes found; skipping labelling."
    else
        for NODE in ${NODES}; do
            kubectl label "${NODE}" "${NODE_LABEL_KEY}=${NODE_LABEL_VALUE}" --overwrite
            echo "  labeled ${NODE}"
        done
    fi
else
    echo "### Step 4: Skipped (--no-label) ###"
fi

#############################################################################
# Step 5: Verify installation
#############################################################################
echo "### Step 5: Verifying installation ###"
echo "--- RuntimeClasses ---"
kubectl get runtimeclasses
echo ""
echo "--- Labeled nodes ---"
kubectl get nodes -l "${NODE_LABEL_KEY}=${NODE_LABEL_VALUE}" -o wide || true
echo ""
echo "### Setup complete! ###"
echo "You can now deploy Firecracker-based Agent Sandboxes using RuntimeClass '${RUNTIME_CLASS_NAME}'."
