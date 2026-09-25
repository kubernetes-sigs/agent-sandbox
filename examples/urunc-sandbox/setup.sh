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

# Setup script for urunc on a Kubernetes cluster.
# Installs urunc-deploy (RBAC + DaemonSet), registers the urunc RuntimeClass,
# and waits for the nodes to be labeled by the installer.
#
# Re-running this on a cluster that already has urunc is safe; every step is idempotent.
#
# The script uses whatever kubectl context is currently configured. On k3s the default
# kubeconfig (/etc/rancher/k3s/k3s.yaml) is readable only by root, so run this under sudo
# or export KUBECONFIG to a readable copy.

set -euo pipefail

# Defaults
#
# Pinned to the commit tagged v0.8.0, the latest stable urunc release. A SHA
# rather than "nightly" or the tag itself: both are mutable, and this ref is
# interpolated straight into the `kubectl apply -k` URLs below, so whatever it
# resolves to at run time is applied to the cluster with the caller's
# privileges. Override with --urunc-version to track a newer revision.
URUNC_VERSION="27f3d069100e9bca0ee063257e92694a0a5455e4"
RUNTIME_CLASS_NAME="urunc"
NODE_LABEL_KEY="urunc.io/urunc-runtime"
NODE_LABEL_VALUE="true"
FLAVOR="base"
INSTALL_URUNC=true
CHECK_KVM=true
UNINSTALL=false

usage() {
    cat <<'USAGE'
Usage: ./setup.sh [OPTIONS...]

  --urunc-version <ref>   Git ref of urunc-dev/urunc to install from
                          (default: the commit tagged v0.8.0)
  --flavor <base|k3s>     Kustomize overlay to use. Use "k3s" on k3s clusters,
                          which keep containerd's config outside /etc/containerd
                          (default: base)
  --runtime-class-name    RuntimeClass name to register (default: urunc)
  --no-install            Skip urunc-deploy; only register the RuntimeClass
  --skip-kvm-check        Do not verify /dev/kvm on the local host
  --uninstall             Remove urunc from the nodes and delete the
                          RuntimeClass and RBAC, then exit
  -h, --help              Show this message
USAGE
}

# Called as `require_value "$@"` from inside the loop, so $1 is the option and $2
# its value, if one was given. Without this, `set -u` turns a missing value into
# "$2: unbound variable", which points at the script instead of at the command
# line that caused it.
require_value() {
    if [[ $# -lt 2 || -z "$2" ]]; then
        echo "Error: $1 requires a value."
        usage
        exit 1
    fi
}

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --urunc-version)      require_value "$@"; URUNC_VERSION="$2"; shift ;;
        --flavor)             require_value "$@"; FLAVOR="$2"; shift ;;
        --runtime-class-name) require_value "$@"; RUNTIME_CLASS_NAME="$2"; shift ;;
        --no-install)         INSTALL_URUNC=false ;;
        --skip-kvm-check)     CHECK_KVM=false ;;
        --uninstall)          UNINSTALL=true ;;
        -h|--help)            usage; exit 0 ;;
        *) echo "Unknown parameter passed: $1"; usage; exit 1 ;;
    esac
    shift
done

if [[ "${FLAVOR}" != "base" && "${FLAVOR}" != "k3s" ]]; then
    echo "Error: --flavor must be 'base' or 'k3s' (got '${FLAVOR}')."
    exit 1
fi

# Validate the ref so it cannot be used to escape the kustomize URL path.
if [[ ! "${URUNC_VERSION}" =~ ^[0-9A-Za-z._/-]+$ ]]; then
    echo "Error: Invalid urunc version/ref: '${URUNC_VERSION}'"
    exit 1
fi

URUNC_REPO="https://github.com/urunc-dev/urunc"

if [[ "${FLAVOR}" == "k3s" ]]; then
    DEPLOY_PATH="deployment/urunc-deploy/urunc-deploy/overlays/k3s"
    CLEANUP_PATH="deployment/urunc-deploy/urunc-cleanup/overlays/k3s"
else
    DEPLOY_PATH="deployment/urunc-deploy/urunc-deploy/base"
    CLEANUP_PATH="deployment/urunc-deploy/urunc-cleanup/base"
fi

#############################################################################
# Uninstall
#
# Follows the teardown sequence documented upstream at
# https://urunc.io/tutorials/How-to-urunc-on-k8s/#urunc-deploy-in-k3s
# (delete urunc-deploy, run urunc-cleanup, then the RuntimeClass and the RBAC),
# with two additions noted at the relevant steps below.
#
# Removing urunc is a two-stage handoff between two DaemonSets, coordinated
# through a node label:
#
#   1. Deleting urunc-deploy fires its preStop hook, which restores the
#      containerd configuration, removes the binaries and monitors, and
#      re-labels the node urunc.io/urunc-runtime=cleanup.
#   2. That label is the nodeSelector of urunc-cleanup, which then removes the
#      label, reloads the container runtime and waits for the node to be Ready.
#
# Stopping after stage 1 leaves the node labeled "cleanup" with a runtime that
# was never reloaded. Both DaemonSets use the urunc-deploy-sa ServiceAccount,
# so the RBAC has to outlive stage 2 and is deleted last. Neither stage touches
# the RuntimeClass -- urunc-deploy has no reference to it -- so this script,
# having created it, removes it here.
#############################################################################
if [[ "${UNINSTALL}" == "true" ]]; then
    echo "### Uninstalling urunc (flavor: ${FLAVOR}, ref: ${URUNC_VERSION}) ###"

    echo "--- Stage 1: removing urunc-deploy ---"
    kubectl delete -k "${URUNC_REPO}/${DEPLOY_PATH}?ref=${URUNC_VERSION}" --ignore-not-found

    echo "--- Stage 2: running urunc-cleanup ---"
    # urunc-cleanup runs as urunc-deploy-sa. Re-apply the RBAC first: on a node
    # left half-uninstalled (urunc-deploy deleted by hand, or a previous run
    # interrupted) the ServiceAccount may already be gone, and the DaemonSet
    # controller then refuses to create the pod with "error looking up service
    # account", which only surfaces as a rollout timeout.
    kubectl apply -k "${URUNC_REPO}/deployment/urunc-deploy/urunc-rbac?ref=${URUNC_VERSION}"
    kubectl apply -k "${URUNC_REPO}/${CLEANUP_PATH}?ref=${URUNC_VERSION}"
    # The upstream sequence applies and then deletes urunc-cleanup with nothing
    # in between, which is fine when a human runs the commands one at a time but
    # races when scripted: deleting the DaemonSet before its pod has run leaves
    # the node still labeled "cleanup" and the container runtime not reloaded.
    if ! kubectl -n kube-system rollout status daemonset/kubelet-urunc-cleanup --timeout=5m; then
        echo ""
        echo "Error: urunc-cleanup did not roll out. Recent events:"
        kubectl -n kube-system describe daemonset/kubelet-urunc-cleanup | sed -n "/Events:/,\$p"
        exit 1
    fi
    kubectl delete -k "${URUNC_REPO}/${CLEANUP_PATH}?ref=${URUNC_VERSION}" --ignore-not-found

    # Deleted by name rather than through the upstream runtimeclass.yaml: this
    # script registers its own RuntimeClass with a scheduling nodeSelector, and
    # honours --runtime-class-name.
    echo "--- Removing the RuntimeClass and RBAC ---"
    kubectl delete runtimeclass "${RUNTIME_CLASS_NAME}" --ignore-not-found
    kubectl delete -k "${URUNC_REPO}/deployment/urunc-deploy/urunc-rbac?ref=${URUNC_VERSION}" --ignore-not-found

    echo ""
    echo "### Uninstall complete ###"
    echo "Nodes still labeled '${NODE_LABEL_KEY}' (should be empty):"
    kubectl get nodes -l "${NODE_LABEL_KEY}" --no-headers || true
    exit 0
fi

echo "### Configuration ###"
echo "URUNC_VERSION:       ${URUNC_VERSION}"
echo "FLAVOR:              ${FLAVOR}"
echo "RUNTIME_CLASS_NAME:  ${RUNTIME_CLASS_NAME}"
echo "INSTALL_URUNC:       ${INSTALL_URUNC}"
echo "#####################"
echo ""

#############################################################################
# Step 1: Verify KVM support
#############################################################################
echo "### Step 1: Verifying KVM support ###"
if [[ "${CHECK_KVM}" == "true" ]]; then
    if [[ ! -e /dev/kvm ]]; then
        echo "Error: /dev/kvm not found. urunc boots workloads inside a VMM"
        echo "(QEMU, Firecracker, Cloud Hypervisor) and needs hardware virtualization."
        echo "Ensure the host CPU supports VT-x/AMD-V and that KVM modules are loaded."
        echo "Try: sudo modprobe kvm && sudo modprobe kvm_intel (or kvm_amd)"
        echo ""
        echo "When this script runs from a workstation that drives a remote cluster"
        echo "whose nodes do have KVM, pass --skip-kvm-check to bypass."
        exit 1
    fi
    echo "### KVM support detected at /dev/kvm ###"
    echo ""
    echo "Note: this check runs on the local machine only. Ensure every target node"
    echo "      has /dev/kvm before scheduling urunc pods onto it."
else
    echo "### Step 1: Skipped (--skip-kvm-check) ###"
fi
echo ""

#############################################################################
# Step 2: Install urunc via urunc-deploy
#############################################################################
if [[ "${INSTALL_URUNC}" == "true" ]]; then
    echo "### Step 2: Installing urunc (urunc-deploy) ###"

    echo "--- Applying urunc RBAC ---"
    kubectl apply -k "${URUNC_REPO}/deployment/urunc-deploy/urunc-rbac?ref=${URUNC_VERSION}"

    echo "--- Applying urunc-deploy DaemonSet (${FLAVOR}) ---"
    kubectl apply -k "${URUNC_REPO}/${DEPLOY_PATH}?ref=${URUNC_VERSION}"

    echo "--- Waiting for urunc-deploy rollout (this may take several minutes) ---"
    echo "    The installer places the urunc binaries and hypervisors on each node,"
    echo "    rewrites the containerd config, and reloads containerd."
    kubectl -n kube-system rollout status daemonset/urunc-deploy --timeout=10m
    echo "### urunc installed ###"
else
    echo "### Step 2: Skipped (--no-install) ###"
fi
echo ""

#############################################################################
# Step 3: Register the urunc RuntimeClass
#############################################################################
echo "### Step 3: Registering RuntimeClass '${RUNTIME_CLASS_NAME}' ###"

cat <<EOF | kubectl apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: ${RUNTIME_CLASS_NAME}
handler: urunc
scheduling:
  nodeSelector:
    kubernetes.io/os: linux
    ${NODE_LABEL_KEY}: "${NODE_LABEL_VALUE}"
EOF

echo "### RuntimeClass '${RUNTIME_CLASS_NAME}' created ###"
echo ""

#############################################################################
# Step 4: Verify installation
#############################################################################
echo "### Step 4: Verifying installation ###"
echo "--- RuntimeClasses ---"
kubectl get runtimeclasses
echo ""
echo "--- Nodes with urunc installed ---"
# urunc-deploy applies this label itself once install.sh finishes on the node.
# An empty list means the DaemonSet has not finished; check its logs with:
#   kubectl logs -n kube-system -l name=urunc-deploy
kubectl get nodes -l "${NODE_LABEL_KEY}=${NODE_LABEL_VALUE}" -o wide || true
echo ""
echo "### Setup complete! ###"
echo "Deploy urunc-based Agent Sandboxes using RuntimeClass '${RUNTIME_CLASS_NAME}'."
echo "Remember: the sandbox image must be built with a kernel attached first — see README.md."
