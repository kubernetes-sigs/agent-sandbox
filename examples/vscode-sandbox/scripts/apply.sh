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
  -h, --help                    Display this help message and exit
  -i, --sandbox-router-image    Specify a custom image for sandbox-router

Examples:
  $(basename "$0") -i repository/custom-sandbox-router:tag
  $(basename "$0") --sandbox-router-image repository/custom-sandbox-router:tag
  $(basename "$0") --help
EOF
}

# parse arguments in case statement and include argument -i or --sandbox-router-image to specify custom image for sandbox-router
while [[ "$#" -gt 0 ]]; do
    case $1 in
        -h|--help) usage; exit 0 ;;
        -i|--sandbox-router-image) SANDBOX_ROUTER_IMAGE="$2"; shift ;;
        *) echo "Unknown parameter passed: $1"; usage; exit 1 ;;
    esac
    shift
done

# if sandbox-router-image is not set, exit with error
if [[ -z "$SANDBOX_ROUTER_IMAGE" ]]; then
    echo "No sandbox-router image specified."
    usage;
    exit 1
fi

# get root directory of this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export KUBECONFIG="${SCRIPT_DIR}/kubeconfig"

# install agent sandbox
export VERSION="v0.1.0"

# install only the core components:
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml

# install the extensions components:
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml

# install sandbox-router
pushd ../../../clients/python/agentic-sandbox-client/sandbox-router
# replace IMAGE_PLACEHOLDER with the actual image
sed "s|IMAGE_PLACEHOLDER|${SANDBOX_ROUTER_IMAGE}|g" ./sandbox_router.yaml | kubectl apply -f -
popd

# install vscode-sandbox with kata-mshv overlay
pushd ../../vscode-sandbox
kubectl apply -k ./overlays/kata-mshv
popd
