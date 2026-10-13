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

# Create the kind cluster with the default CNI (kube-network-policies runs
# alongside kindnet). One worker node so the DaemonSet runs on more than one node.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"

if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
  echo "kind cluster $KIND_CLUSTER_NAME already exists, reusing it."
  exit 0
fi

image_flag=()
if [ -n "$KIND_NODE_IMAGE" ]; then
  image_flag=(--image "$KIND_NODE_IMAGE")
fi

kind create cluster --name "$KIND_CLUSTER_NAME" "${image_flag[@]}" --wait 2m --config=- <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
EOF
echo "OK: kind cluster $KIND_CLUSTER_NAME created."
