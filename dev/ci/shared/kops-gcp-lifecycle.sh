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

# Shared kOps/GCE lifecycle for benchmark and e2e scenario scripts.
#
# Source this file (do not exec) so callers can interleave their own
# kops-edit blocks between kops_create_cluster and kops_apply_and_validate.
#
# Usage:
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   source "${REPO_ROOT}/dev/ci/shared/kops-gcp-lifecycle.sh"
#   kops_install_tools
#   kops_acquire_project
#   kops_register_cleanup  # install early; tolerates resources not yet created
#   kops_setup_images "agent-sandbox-controller"
#   kops_create_bucket
#   kops_create_cluster "sandbox-${UNIQUE_SUFFIX}" "${ZONE}"
#   # ... scenario-specific kops edit calls ...
#   kops_apply_and_validate

REPO_ROOT="${REPO_ROOT:-$(git rev-parse --show-toplevel)}"
cd "${REPO_ROOT}"

export WORKDIR="${REPO_ROOT}/.build"
rm -rf "${WORKDIR}"
mkdir -p "${WORKDIR}/bin"

export BINDIR="${WORKDIR}/bin"
export PATH="${BINDIR}:${PATH}"

export KUBECONFIG="${WORKDIR}/kubeconfig"

UNIQUE_SUFFIX="$(date +%Y%m%d-%H%M%S)"

KOPS_VERSION="${KOPS_VERSION:-v1.36.0}"

kops_install_tools() {
  echo "Installing kOps ${KOPS_VERSION} to ${BINDIR}..."
  GOBIN="${BINDIR}" go install "k8s.io/kops/cmd/kops@${KOPS_VERSION}"

  echo "Installing resourcectl to ${BINDIR}..."
  pushd dev/tools >/dev/null
  GOBIN="${BINDIR}" go install ./resourcectl
  popd >/dev/null
}

kops_acquire_project() {
  if [[ -n "${BOSKOS_HOST:-}" ]]; then
    if ! PROJECT_ID=$(resourcectl get --key project-main --boskos-type gce-project); then
      echo "Failed to acquire project from Boskos"
      exit 1
    fi
    echo "Acquired project ${PROJECT_ID} from Boskos."
    trap 'resourcectl cleanup || true' EXIT
  else
    PROJECT_ID=$(gcloud config get-value project)
  fi

  PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)")
  echo "Using project: ${PROJECT_ID} (number: ${PROJECT_NUMBER})"
}

kops_setup_images() {
  IMAGE_PREFIX="gcr.io/${PROJECT_ID}/agent-sandbox/"
  export IMAGE_PREFIX
  echo "Using image prefix: ${IMAGE_PREFIX}"

  gcloud auth configure-docker --quiet gcr.io

  IMAGE_TAG=$(git describe --tags --dirty --always)
  if [[ "${IMAGE_TAG}" == *"-dirty" ]]; then
    IMAGE_TAG="${IMAGE_TAG}-${UNIQUE_SUFFIX}"
  fi
  export IMAGE_TAG
  echo "Using image tag: ${IMAGE_TAG}"

  for image in "$@"; do
    echo "Pushing ${image} to ${IMAGE_PREFIX}..."
    dev/tools/push-images --image-prefix="${IMAGE_PREFIX}" --platform=linux/amd64 --images "${image}"
  done
}

kops_create_bucket() {
  BUCKET_NAME="kops-state-${PROJECT_NUMBER}"
  echo "Using bucket: ${BUCKET_NAME}"

  if ! gcloud storage buckets describe "gs://${BUCKET_NAME}" >/dev/null 2>&1; then
    echo "Creating bucket gs://${BUCKET_NAME}..."
    gcloud storage buckets create "gs://${BUCKET_NAME}" --project="${PROJECT_ID}" --location=us-central1
  else
    echo "Bucket gs://${BUCKET_NAME} already exists."
  fi

  export KOPS_STATE_STORE="gs://${BUCKET_NAME}"
}

kops_create_cluster() {
  CLUSTER_NAME="$1"
  ZONE="${2:-us-central1-a}"

  local cni="${CNI:-cilium}"
  local node_count="${STRESS_NODE_COUNT:-3}"
  local cp_size="${STRESS_CONTROL_PLANE_SIZE:-n2-standard-4}"
  local node_size="${STRESS_NODE_SIZE:-n2-standard-8}"
  local node_volume_type="${STRESS_NODE_VOLUME_TYPE:-pd-ssd}"

  echo "Creating kOps cluster: ${CLUSTER_NAME} (${node_count} workers, CP=${cp_size}, CNI=${cni})"
  kops create cluster \
    --cloud=gce \
    --gce-service-account=default \
    --networking="${cni}" \
    --name="${CLUSTER_NAME}" \
    --zones="${ZONE}" \
    --project="${PROJECT_ID}" \
    --control-plane-count=1 \
    --control-plane-size="${cp_size}" \
    --node-count="${node_count}" \
    --node-size="${node_size}"

  kops edit instancegroup --name "${CLUSTER_NAME}" "nodes-${ZONE}" \
    --set "spec.rootVolume.type=${node_volume_type}"

  kops edit instancegroup --name "${CLUSTER_NAME}" "control-plane-${ZONE}" \
    --set "spec.rootVolume.type=pd-ssd"
}

kops_apply_and_validate() {
  local wait_time="${1:-${STRESS_VALIDATE_WAIT:-25m}}"

  echo "Applying cluster configuration..."
  kops update cluster --name "${CLUSTER_NAME}" --yes

  echo "Exporting kubeconfig..."
  kops export kubeconfig --name "${CLUSTER_NAME}" --admin

  kops_validate_cluster "${wait_time}"
}

kops_validate_cluster() {
  local wait_time="${1:-${STRESS_VALIDATE_WAIT:-25m}}"

  echo "Waiting for cluster to be ready (timeout: ${wait_time})..."
  if ! kops validate cluster --wait "${wait_time}"; then
    kops_capture_cp_serial_port
    exit 1
  fi
}

kops_capture_cp_serial_port() {
  echo "kOps cluster validation failed. Capturing control plane serial port output..."
  local out_dir="${ARTIFACTS:-.}"
  mkdir -p "${out_dir}"
  local cp_instance
  cp_instance=$(gcloud compute instance-groups managed list-instances \
    "a-control-plane-${ZONE}-${CLUSTER_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --format="value(instance.basename())" 2>/dev/null | head -n1 || true)
  if [[ -z "${cp_instance}" ]]; then
    cp_instance=$(gcloud compute instances list --project="${PROJECT_ID}" \
      --filter="name ~ 'control-plane.*${CLUSTER_NAME}' AND zone:${ZONE}" \
      --format="value(name)" 2>/dev/null | head -n1 || true)
  fi
  if [[ -n "${cp_instance}" ]]; then
    echo "Saving serial port output from ${cp_instance} to ${out_dir}/control-plane-serial-port.log..."
    gcloud compute instances get-serial-port-output "${cp_instance}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      > "${out_dir}/control-plane-serial-port.log" 2>&1 || true
  else
    echo "WARNING: Could not find control-plane instance for ${CLUSTER_NAME}; serial port output was not captured." >&2
  fi
}

kops_register_cleanup() {
  cleanup() {
    echo "Cleaning up..."

    if declare -F "${KOPS_PRE_CLEANUP_HOOK:-}" >/dev/null 2>&1; then
      "${KOPS_PRE_CLEANUP_HOOK}" || true
    fi

    if [[ -n "${CLUSTER_NAME:-}" ]]; then
      kops delete cluster --name="${CLUSTER_NAME}" --yes || true
    fi

    if [[ "${CLEANUP_BUCKET:-false}" == "true" && -n "${BUCKET_NAME:-}" ]]; then
      echo "Deleting bucket gs://${BUCKET_NAME}..."
      gcloud storage buckets delete "gs://${BUCKET_NAME}" --quiet || true
    fi

    echo "Cleaning up resources via resourcectl..."
    resourcectl cleanup || true

    rm -rf "${WORKDIR}"
    echo "DONE"
  }
  trap cleanup EXIT
}
