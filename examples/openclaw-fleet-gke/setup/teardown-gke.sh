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

# Tears down everything created by provision-gke.sh: the GKE cluster, any
# Filestore instances that the Filestore CSI driver provisioned for the
# example's PVCs, and the snapshot bucket.
#
# Destructive. Each deletion prompts for confirmation unless FORCE=true.
set -euo pipefail

# --- Configuration (override via environment; must match provision-gke.sh) --
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
ZONE="${ZONE:-us-central1-a}"
CLUSTER_NAME="${CLUSTER_NAME:-openclaw-fleet-poc}"
SNAPSHOT_BUCKET="${SNAPSHOT_BUCKET:-${PROJECT_ID}-openclaw-fleet-snapshots}"
NAMESPACE="${NAMESPACE:-openclaw-fleet}"
FORCE="${FORCE:-false}"

if [[ -z "${PROJECT_ID}" || "${PROJECT_ID}" == "(unset)" ]]; then
    echo "Error: PROJECT_ID is not set and no default gcloud project is configured." >&2
    exit 1
fi

confirm() { # confirm <prompt> -> 0 if confirmed (or FORCE=true), 1 otherwise
    if [[ "${FORCE}" == "true" ]]; then
        return 0
    fi
    local reply
    read -r -p "$1 [y/N] " reply
    [[ "${reply}" =~ ^[Yy]$ ]]
}

echo "### Teardown configuration ###"
echo "PROJECT_ID:      ${PROJECT_ID}"
echo "ZONE:            ${ZONE}"
echo "CLUSTER_NAME:    ${CLUSTER_NAME}"
echo "SNAPSHOT_BUCKET: ${SNAPSHOT_BUCKET}"
echo "NAMESPACE:       ${NAMESPACE}"
echo "FORCE:           ${FORCE}"
echo "##############################"

echo "### Step 1: Deleting GKE cluster '${CLUSTER_NAME}' ###"
if gcloud container clusters describe "${CLUSTER_NAME}" \
        --zone "${ZONE}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    if confirm "Delete cluster '${CLUSTER_NAME}' in ${ZONE}?"; then
        gcloud container clusters delete "${CLUSTER_NAME}" \
            --zone "${ZONE}" --project "${PROJECT_ID}" --quiet
    else
        echo "--- Skipping cluster deletion ---"
    fi
else
    echo "--- Cluster '${CLUSTER_NAME}' not found; nothing to delete ---"
fi

echo "### Step 2: Deleting Filestore instances provisioned for the example ###"
# The Filestore CSI driver labels every instance it provisions with the PVC
# that requested it. Filter on the PVC namespace so only this example's
# instances are matched. Deleting the cluster does NOT delete these — and
# they contain tenant workspace data, so review the list before confirming.
FILESTORE_INSTANCES="$(gcloud filestore instances list \
    --project "${PROJECT_ID}" \
    --filter "labels.kubernetes_io_created-for_pvc_namespace=${NAMESPACE}" \
    --format "value(name)" 2>/dev/null || true)"
if [[ -z "${FILESTORE_INSTANCES}" ]]; then
    echo "--- No Filestore instances labeled for namespace '${NAMESPACE}' ---"
else
    echo "WARNING: the following Filestore instances hold user/tenant data:"
    echo "${FILESTORE_INSTANCES}"
    while read -r instance; do
        [[ -z "${instance}" ]] && continue
        if confirm "Delete Filestore instance '${instance}' (data is unrecoverable)?"; then
            # 'name' from the list output is the full resource path
            # (projects/.../locations/.../instances/...), which gcloud accepts.
            gcloud filestore instances delete "${instance}" \
                --project "${PROJECT_ID}" --quiet
        else
            echo "--- Skipping ${instance} ---"
        fi
    done <<< "${FILESTORE_INSTANCES}"
fi

echo "### Step 3: Deleting snapshot bucket gs://${SNAPSHOT_BUCKET} ###"
if gcloud storage buckets describe "gs://${SNAPSHOT_BUCKET}" \
        --project "${PROJECT_ID}" >/dev/null 2>&1; then
    if confirm "Delete bucket gs://${SNAPSHOT_BUCKET} and ALL snapshots in it?"; then
        gcloud storage rm --recursive "gs://${SNAPSHOT_BUCKET}"
    else
        echo "--- Skipping bucket deletion ---"
    fi
else
    echo "--- Bucket gs://${SNAPSHOT_BUCKET} not found; nothing to delete ---"
fi

echo "### Teardown complete ###"
