#!/usr/bin/env bash
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


set -o errexit
set -o nounset
set -o pipefail

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

This script applies the necessary Kubernetes manifests to set up the VSCode sandbox environment.

Options:
  -h, --help                        Display this help message and exit
  -i, --sandbox-router-image IMAGE  Specify a custom image for sandbox-router
      --kubeconfig FILE             Select a kubeconfig (default: kubeconfig next to this script)

Examples:
  $(basename "$0") -i repository/custom-sandbox-router:tag
  $(basename "$0") -i repository/custom-sandbox-router:tag --kubeconfig ./aks-kubeconfig
  $(basename "$0") --help
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KUBECONFIG_FILE="${SCRIPT_DIR}/kubeconfig"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        -h|--help) usage; exit 0 ;;
        -i|--sandbox-router-image)
            if [[ "$#" -lt 2 || -z "$2" || "$2" == -* ]]; then
                echo "$1 requires an image value." >&2
                usage
                exit 1
            fi
            SANDBOX_ROUTER_IMAGE="$2"
            shift
            ;;
        --kubeconfig)
            if [[ "$#" -lt 2 || -z "$2" || "$2" == -* ]]; then
                echo "$1 requires a file path." >&2
                usage
                exit 1
            fi
            KUBECONFIG_FILE="$2"
            shift
            ;;
        *) echo "Unknown parameter passed: $1"; usage; exit 1 ;;
    esac
    shift
done

if [[ -z "${SANDBOX_ROUTER_IMAGE:-}" ]]; then
    echo "No sandbox-router image specified." >&2
    usage
    exit 1
fi

REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Select one cluster explicitly because the Router installation grants cluster-wide RBAC.
kubectl_cmd=(kubectl --kubeconfig "$KUBECONFIG_FILE")

VERSION="${VERSION:-v1.0.12}"

# install only the core components:
"${kubectl_cmd[@]}" apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox.yaml"

# install the extensions components:
"${kubectl_cmd[@]}" apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml"

ROUTER_DIR="${REPO_ROOT}/sandbox-router/deploy"
"${kubectl_cmd[@]}" apply -f "${ROUTER_DIR}/serviceaccount.yaml" \
    -f "${ROUTER_DIR}/rbac.yaml" \
    -f "${ROUTER_DIR}/service.yaml"
# Set the image before applying so the deployment never pulls the placeholder tag.
"${kubectl_cmd[@]}" set image --local -f "${ROUTER_DIR}/deployment.yaml" \
    "sandbox-router=${SANDBOX_ROUTER_IMAGE}" -o yaml | "${kubectl_cmd[@]}" apply -f -

"${kubectl_cmd[@]}" apply -k "${SCRIPT_DIR}/../overlays/kata-mshv"
