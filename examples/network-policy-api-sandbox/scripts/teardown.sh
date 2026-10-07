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

# Teardown. By default removes only the demo resources (keeps the cluster,
# agent-sandbox and kube-network-policies). Pass --all to delete the kind cluster.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"

if [ "${1:-}" = "--all" ]; then
  echo "Deleting kind cluster $KIND_CLUSTER_NAME ..."
  kind delete cluster --name "$KIND_CLUSTER_NAME"
  echo "Cluster deleted."
  exit 0
fi

echo "Removing demo resources ..."
if kubectl get crd clusternetworkpolicies.policy.networking.k8s.io >/dev/null 2>&1; then
  for f in "${CNP_MANIFESTS[@]}"; do
    kubectl delete -f "$M/$f.yaml" --ignore-not-found
  done
fi
kubectl delete -f "$M/90-raw-sandbox.yaml" --ignore-not-found
kubectl delete -f "$M/20-sandbox-templates.yaml" --ignore-not-found
kubectl delete -f "$M/10-shared-tools.yaml" --ignore-not-found
kubectl delete -f "$M/00-namespaces.yaml" --ignore-not-found
echo "Demo resources removed. Cluster left running. Use '--all' to delete the kind cluster too."
