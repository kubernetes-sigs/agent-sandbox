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

# Provisions the GKE Standard cluster for the openclaw-fleet-gke example:
#   - gVisor node pool (warm pool + Pod Snapshots need it; Pod Snapshots
#     additionally require non-E2 machine types)
#   - Filestore CSI driver (late-bound RWX tenant storage; Standard-only
#     because the storage node daemon is a privileged DaemonSet)
#   - Workload Identity + a hierarchical-namespace GCS bucket (Pod Snapshots)
#   - Gateway API (sandbox routing)
#   - Image streaming (image-cache acceleration; optional secondary boot
#     disk sketched at the bottom of this file)
#   - agent-sandbox controller + extensions from the release manifests
#
# Every variable below is env-overridable; the script is idempotent where
# practical (existing cluster / node pool / bucket / namespace are reused).
set -euo pipefail

# --- Configuration (override via environment) -------------------------------
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
REGION="${REGION:-us-central1}"
ZONE="${ZONE:-us-central1-a}"
CLUSTER_NAME="${CLUSTER_NAME:-openclaw-fleet-poc}"
# c3-standard-8 matches the series the measured results were taken on. Pod
# Snapshots (memory-tier sleep) restore only onto the SAME machine series
# (and never E2), so pick one series for the whole fleet and keep it.
GVISOR_MACHINE_TYPE="${GVISOR_MACHINE_TYPE:-c3-standard-8}"
GVISOR_NODES="${GVISOR_NODES:-3}"
SNAPSHOT_BUCKET="${SNAPSHOT_BUCKET:-${PROJECT_ID}-openclaw-fleet-snapshots}"
# Minimum 1.36.0-gke.3302001: first GKE version that supports the gVisor
# annotation dev.gvisor.empty-dir.<name>.force-shared="true", which the
# late-binding storage daemon needs to bind-mount Filestore subdirectories
# into a running sandbox's emptyDir. "1.36" lets GKE pick the newest patch
# on the channel that satisfies the minor version.
CLUSTER_VERSION="${CLUSTER_VERSION:-1.36}"
RELEASE_CHANNEL="${RELEASE_CHANNEL:-rapid}"
# "latest" or a pinned release tag such as "v1.0.2".
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-latest}"
NAMESPACE="${NAMESPACE:-openclaw-fleet}"
ENABLE_SECONDARY_BOOT_DISK="${ENABLE_SECONDARY_BOOT_DISK:-false}"

if [[ -z "${PROJECT_ID}" || "${PROJECT_ID}" == "(unset)" ]]; then
    echo "Error: PROJECT_ID is not set and no default gcloud project is configured." >&2
    echo "Run 'gcloud config set project <id>' or export PROJECT_ID." >&2
    exit 1
fi

echo "### Configuration ###"
echo "PROJECT_ID:            ${PROJECT_ID}"
echo "REGION:                ${REGION}"
echo "ZONE:                  ${ZONE}"
echo "CLUSTER_NAME:          ${CLUSTER_NAME}"
echo "CLUSTER_VERSION:       ${CLUSTER_VERSION} (channel: ${RELEASE_CHANNEL})"
echo "GVISOR_MACHINE_TYPE:   ${GVISOR_MACHINE_TYPE}"
echo "GVISOR_NODES:          ${GVISOR_NODES}"
echo "SNAPSHOT_BUCKET:       ${SNAPSHOT_BUCKET}"
echo "AGENT_SANDBOX_VERSION: ${AGENT_SANDBOX_VERSION}"
echo "NAMESPACE:             ${NAMESPACE}"
echo "######################"

echo "### Step 1: Enabling required Google Cloud APIs ###"
gcloud services enable \
    container.googleapis.com \
    file.googleapis.com \
    storage.googleapis.com \
    --project "${PROJECT_ID}"

echo "### Step 2: Creating GKE Standard cluster '${CLUSTER_NAME}' ###"
if gcloud container clusters describe "${CLUSTER_NAME}" \
        --zone "${ZONE}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    echo "--- Cluster '${CLUSTER_NAME}' already exists in ${ZONE}; reusing ---"
else
    # Default pool hosts system pods and the non-sandboxed example
    # components (controller, router, storage daemon); e2-standard-4 is
    # plenty. Pod Snapshots and gVisor run on the dedicated pool below.
    gcloud container clusters create "${CLUSTER_NAME}" \
        --project "${PROJECT_ID}" \
        --zone "${ZONE}" \
        --release-channel "${RELEASE_CHANNEL}" \
        --cluster-version "${CLUSTER_VERSION}" \
        --image-type COS_CONTAINERD \
        --machine-type e2-standard-4 \
        --num-nodes 1 \
        --enable-image-streaming \
        --workload-pool "${PROJECT_ID}.svc.id.goog" \
        --gateway-api standard \
        --enable-dataplane-v2 \
        --addons GcpFilestoreCsiDriver,HttpLoadBalancing,HorizontalPodAutoscaling
fi

echo "--- Fetching cluster credentials ---"
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --zone "${ZONE}" --project "${PROJECT_ID}"

echo "### Step 3: Creating gVisor node pool ###"
# Pod Snapshots require gVisor sandboxed nodes AND a non-E2 machine type
# (hence c3-standard-8 by default; restores also require the SAME series,
# so keep the whole fleet on one). Image streaming keeps first pulls of
# the OpenClaw image fast on freshly scaled nodes.
if gcloud container node-pools describe gvisor-pool \
        --cluster "${CLUSTER_NAME}" --zone "${ZONE}" \
        --project "${PROJECT_ID}" >/dev/null 2>&1; then
    echo "--- Node pool 'gvisor-pool' already exists; reusing ---"
else
    gcloud container node-pools create gvisor-pool \
        --project "${PROJECT_ID}" \
        --cluster "${CLUSTER_NAME}" \
        --zone "${ZONE}" \
        --machine-type "${GVISOR_MACHINE_TYPE}" \
        --num-nodes "${GVISOR_NODES}" \
        --image-type COS_CONTAINERD \
        --enable-image-streaming \
        --sandbox type=gvisor
fi

echo "### Step 4: Creating snapshot bucket gs://${SNAPSHOT_BUCKET} ###"
# Pod Snapshots require hierarchical namespace; soft delete is disabled
# because snapshot churn plus soft-delete retention inflates storage cost.
if gcloud storage buckets describe "gs://${SNAPSHOT_BUCKET}" \
        --project "${PROJECT_ID}" >/dev/null 2>&1; then
    echo "--- Bucket gs://${SNAPSHOT_BUCKET} already exists; reusing ---"
else
    gcloud storage buckets create "gs://${SNAPSHOT_BUCKET}" \
        --project "${PROJECT_ID}" \
        --location "${REGION}" \
        --uniform-bucket-level-access \
        --enable-hierarchical-namespace \
        --soft-delete-duration=0
fi

echo "### Step 5: Installing agent-sandbox controller + extensions ###"
# Release manifests per the repo README: sandbox-with-extensions.yaml
# bundles the core Sandbox controller and the SandboxClaim / SandboxTemplate /
# SandboxWarmPool extension controllers.
if [[ "${AGENT_SANDBOX_VERSION}" == "latest" ]]; then
    MANIFEST_URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox-with-extensions.yaml"
else
    MANIFEST_URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml"
fi
echo "--- Applying ${MANIFEST_URL} ---"
kubectl apply -f "${MANIFEST_URL}"

echo "--- Waiting for controller deployments to become available ---"
kubectl get deploy -n agent-sandbox-system -o name | while read -r deploy; do
    kubectl rollout status "${deploy}" -n agent-sandbox-system --timeout=300s
done

echo "### Step 6: Creating namespace '${NAMESPACE}' ###"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# --- OPTIONAL: secondary boot disk for image preloading ---------------------
# Image streaming (enabled above) already accelerates cold pulls, but a
# secondary boot disk removes the pull entirely by preloading the OpenClaw
# image into a disk image attached to every node in the pool. Disabled by
# default; the sketch below is directionally correct — follow the GKE docs
# for the authoritative flow:
#   https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading
#
# 1. Build a disk image containing the OpenClaw container image using
#    gke-disk-image-builder (https://github.com/GoogleCloudPlatform/ai-on-gke/tree/main/tools/gke-disk-image-builder):
#      go run ./cli \
#        --project-name=${PROJECT_ID} \
#        --image-name=openclaw-boot-disk \
#        --zone=${ZONE} \
#        --gcs-path=gs://<scratch-bucket>/disk-image-builder \
#        --container-image=ghcr.io/openclaw/openclaw:2026.3.23
#
# 2. Grant the cluster's node service account roles/compute.imageUser on the
#    disk image, and enable the image cache feature on the cluster if needed.
#
# 3. Recreate (or create a parallel) gVisor node pool with the disk attached:
#      gcloud container node-pools create gvisor-pool-preloaded \
#        --cluster=${CLUSTER_NAME} --zone=${ZONE} \
#        --machine-type=${GVISOR_MACHINE_TYPE} --num-nodes=${GVISOR_NODES} \
#        --image-type=COS_CONTAINERD --enable-image-streaming \
#        --sandbox type=gvisor \
#        --secondary-boot-disk=disk-image=projects/${PROJECT_ID}/global/images/openclaw-boot-disk,mode=CONTAINER_IMAGE_CACHE
if [[ "${ENABLE_SECONDARY_BOOT_DISK}" == "true" ]]; then
    echo "ENABLE_SECONDARY_BOOT_DISK=true, but this script does not automate it." >&2
    echo "Follow the commented sketch above and the GKE image-preloading docs." >&2
    exit 1
fi

echo "### Setup complete ###"
echo ""
echo "Next steps:"
echo "  1. Grant the sandbox ServiceAccount access to gs://${SNAPSHOT_BUCKET}"
echo "     via Workload Identity before enabling snapshots (see 60-snapshots/)."
echo "  2. Deploy the warm pool, storage daemon, and router manifests from the"
echo "     example root into namespace '${NAMESPACE}'."
echo "  3. When finished, run setup/teardown-gke.sh to delete everything."
