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

# Deploy the workloads (phase 1 of the README): namespaces, the tool server, and
# a SandboxTemplate + WarmPool + Claim per tenant. No ClusterNetworkPolicy is
# applied here; the README and test.sh add them one at a time.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"

# A previous walkthrough or test.sh run leaves the policies and the raw sandbox
# behind; remove this example's own objects so the demo starts in phase 1.
if kubectl get crd clusternetworkpolicies.policy.networking.k8s.io >/dev/null 2>&1; then
  for f in "${CNP_MANIFESTS[@]}"; do
    kubectl delete -f "$M/$f.yaml" --ignore-not-found
  done
fi
kubectl delete -f "$M/90-raw-sandbox.yaml" --ignore-not-found

kubectl apply -f "$M/00-namespaces.yaml"
kubectl apply -f "$M/10-shared-tools.yaml"
kubectl apply -f "$M/20-sandbox-templates.yaml"

echo "Waiting for the tool server and the sandbox claims to be Ready..."
kubectl rollout status deployment/tool-server -n "$NS_TOOLS" --timeout=120s
kubectl wait --for=condition=Ready sandboxclaim/agent -n "$NS_A" --timeout=180s
kubectl wait --for=condition=Ready sandboxclaim/agent -n "$NS_B" --timeout=180s

echo
echo "Template-managed NetworkPolicies (the NetworkPolicy tier):"
kubectl get networkpolicy -A
echo
echo "Sandbox pods (the ClusterNetworkPolicies select on the agents.x-k8s.io/sandbox-name-hash label):"
kubectl get pods -A -l agents.x-k8s.io/sandbox-name-hash -L agents.x-k8s.io/sandbox-name-hash
echo "OK: workloads deployed. No ClusterNetworkPolicy applied yet."
