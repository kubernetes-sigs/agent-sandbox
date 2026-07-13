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

# lib.sh holds helpers shared by the create-cluster, run-tests and teardown
# phase scripts. It is meant to be *sourced*, not executed, so it intentionally
# does not set -e; each phase script owns its own `set -o errexit ...`.

# Pin kOps so results are comparable across runs.
KOPS_VERSION="${KOPS_VERSION:-v1.35.0}"

# SCENARIO_DIR is the directory containing the phase scripts and this lib.
SCENARIO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "${SCENARIO_DIR}" rev-parse --show-toplevel)"

# Persistent per-run workdir. Unlike the old monolithic script we do NOT wipe
# this between phases: create-cluster writes the kubeconfig and cluster.env here
# and run-tests/teardown read them back.
WORKDIR="${WORKDIR:-${REPO_ROOT}/.build}"
BINDIR="${BINDIR:-${WORKDIR}/bin}"

log() { echo "[$(basename "$0")] $*"; }
die() { echo "[$(basename "$0")] ERROR: $*" >&2; exit 1; }

# state_dir <cluster-name> prints the per-cluster state directory.
state_dir() {
  local name="$1"
  [[ -n "${name}" ]] || die "state_dir: cluster name required"
  echo "${WORKDIR}/clusters/${name}"
}

# install_tools installs kOps and resourcectl into BINDIR and puts it on PATH.
install_tools() {
  mkdir -p "${BINDIR}"
  export PATH="${BINDIR}:${PATH}"

  if [[ ! -x "${BINDIR}/kops" ]]; then
    log "Installing kOps ${KOPS_VERSION} to ${BINDIR}..."
    GOBIN="${BINDIR}" go install "k8s.io/kops/cmd/kops@${KOPS_VERSION}"
  fi

  if [[ ! -x "${BINDIR}/resourcectl" ]]; then
    log "Installing resourcectl to ${BINDIR}..."
    ( cd "${REPO_ROOT}/dev/tools" && GOBIN="${BINDIR}" go install ./resourcectl )
  fi
}

# resolve_project sets PROJECT_ID and PROJECT_NUMBER, acquiring a project from
# Boskos when running in CI ($BOSKOS_HOST set), otherwise using the active
# gcloud project. Requires install_tools to have run when using Boskos.
resolve_project() {
  if [[ -n "${BOSKOS_HOST:-}" ]]; then
    if ! PROJECT_ID="$(resourcectl get --key project-main --boskos-type gce-project)"; then
      die "Failed to acquire project from Boskos"
    fi
    log "Acquired project ${PROJECT_ID} from Boskos."
  else
    PROJECT_ID="$(gcloud config get-value project)"
    [[ -n "${PROJECT_ID}" && "${PROJECT_ID}" != "(unset)" ]] || \
      die "No GCP project set; run 'gcloud config set project <id>' or set BOSKOS_HOST"
  fi
  PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)")"
  export PROJECT_ID PROJECT_NUMBER
  log "Using project: ${PROJECT_ID} (number: ${PROJECT_NUMBER})"
}

# configure_images sets IMAGE_PREFIX and IMAGE_TAG and configures docker auth.
# Depends on PROJECT_ID (call resolve_project first).
configure_images() {
  [[ -n "${PROJECT_ID:-}" ]] || die "configure_images: resolve_project must run first"

  IMAGE_PREFIX="gcr.io/${PROJECT_ID}/agent-sandbox/"
  export IMAGE_PREFIX
  log "Using image prefix: ${IMAGE_PREFIX}"

  # Tag from the git description; add a timestamp when the tree is dirty so
  # repeated dirty builds don't collide on a cached image.
  IMAGE_TAG="$(git -C "${REPO_ROOT}" describe --tags --dirty --always)"
  if [[ "${IMAGE_TAG}" == *"-dirty" ]]; then
    IMAGE_TAG="${IMAGE_TAG}-$(date +%Y%m%d-%H%M%S)"
  fi
  export IMAGE_TAG
  log "Using image tag: ${IMAGE_TAG}"

  # Configure Docker to use gcloud credential helper for gcr.io.
  gcloud auth configure-docker --quiet gcr.io
}

# ensure_state_store creates (if needed) the kOps state bucket and exports
# KOPS_STATE_STORE. Depends on PROJECT_ID/PROJECT_NUMBER.
ensure_state_store() {
  [[ -n "${PROJECT_NUMBER:-}" ]] || die "ensure_state_store: resolve_project must run first"

  local bucket="kops-state-${PROJECT_NUMBER}"
  if ! gcloud storage buckets describe "gs://${bucket}" >/dev/null 2>&1; then
    log "Creating bucket gs://${bucket}..."
    gcloud storage buckets create "gs://${bucket}" --project="${PROJECT_ID}" --location=us-central1
  else
    log "Bucket gs://${bucket} already exists."
  fi
  export KOPS_STATE_STORE="gs://${bucket}"
  log "Using state store: ${KOPS_STATE_STORE}"
}

# resolve_config maps a --config value to a kOps manifest path. A bare name maps
# to configs/<name>.yaml; an explicit path is used as-is.
resolve_config() {
  local ref="$1"
  local path
  if [[ "${ref}" == */* || "${ref}" == *.yaml ]]; then
    path="${ref}"
  else
    path="${SCENARIO_DIR}/configs/${ref}.yaml"
  fi
  [[ -f "${path}" ]] || die "config not found: ${ref} (looked for ${path})"
  echo "${path}"
}

# cluster_name_from_manifest prints the Cluster object's metadata.name from a
# kOps manifest. The Cluster doc comes first; its metadata.name is the first
# line indented exactly two spaces as "name:" after the "kind: Cluster" line.
cluster_name_from_manifest() {
  local path="$1"
  local name
  name="$(awk '/^kind: Cluster[[:space:]]*$/{f=1} f && /^  name:[[:space:]]/{print $2; exit}' "${path}")"
  [[ -n "${name}" ]] || die "could not find Cluster metadata.name in ${path}"
  echo "${name}"
}

# load_cluster_env sources the cluster.env written by create-cluster and exports
# KUBECONFIG for the cluster. Fails clearly if the cluster state is missing.
load_cluster_env() {
  local name="$1"
  local dir
  dir="$(state_dir "${name}")"
  [[ -f "${dir}/cluster.env" ]] || \
    die "no state for cluster '${name}' at ${dir}; run create-cluster first"
  # set -a: export everything sourced from cluster.env. Child processes (e.g.
  # test-e2e) read IMAGE_TAG/IMAGE_PREFIX from the environment; a plain source
  # would leave them shell-local and test-e2e would generate a different image
  # tag than the one create-cluster pushed.
  set -a
  # shellcheck disable=SC1091
  source "${dir}/cluster.env"
  set +a
  export KUBECONFIG="${dir}/kubeconfig"
  [[ -f "${KUBECONFIG}" ]] || die "kubeconfig missing at ${KUBECONFIG}"
}
