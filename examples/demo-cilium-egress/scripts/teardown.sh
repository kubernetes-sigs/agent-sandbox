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

# Teardown. By default removes only the demo resources (keeps the cluster).
# Pass --all to also delete the GKE cluster.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"

echo "Removing demo resources from namespace $NAMESPACE ..."
# Optional add-ons (observability + ingress): only attempt the CRD-backed deletes
# when their CRDs are installed, so a missing optional kind is skipped while real
# auth/API/manifest errors still fail the script (-e). --ignore-not-found covers
# absent resources; the router Deployment/Service are core kinds (no guard needed).
if kubectl get crd rules.monitoring.googleapis.com >/dev/null 2>&1; then
  kubectl delete -f "$M/observability/clusterrules.yaml" --ignore-not-found
  kubectl delete -f "$M/observability/clusterpodmonitoring.yaml" --ignore-not-found
fi
if kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1; then
  kubectl delete -f "$M/router/gateway.yaml" --ignore-not-found
fi
kubectl -n "$NAMESPACE" delete deploy/sandbox-router-deployment svc/sandbox-router-svc --ignore-not-found
kubectl -n "$NAMESPACE" delete secret sandbox-router-auth --ignore-not-found
kubectl delete -f "$M/40-cilium-team-b-allow-github.yaml" --ignore-not-found
kubectl delete -f "$M/30-sandboxes.yaml" --ignore-not-found
kubectl delete -f "$M/20-cilium-team-a-allow-github.yaml" --ignore-not-found
kubectl delete -f "$M/10-cilium-allow-dns.yaml" --ignore-not-found
kubectl delete -f "$M/00-namespace-and-identities.yaml" --ignore-not-found

if [ "${1:-}" = "--all" ]; then
  echo "Deleting GKE cluster $CLUSTER in $ZONE ..."
  gcloud container clusters delete "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --quiet
  echo "Cluster deleted."
else
  echo "Demo resources removed. Cluster left running. Use '--all' to delete the cluster too."
fi
