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

# OPTIONAL: visibility into egress-policy violations.
# Enables Google Managed Prometheus + Cilium Hubble metrics, scrapes the Hubble
# policy/drop metrics into Cloud Monitoring, and installs a recording+alert rule.
# Works on a fresh cluster (idempotent). Requires: gcloud, kubectl, cilium CLI.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests/observability"

# 1. Enable Managed Prometheus on the cluster (no-op if already on).
if [ "$(gcloud container clusters describe "$CLUSTER" --zone "$ZONE" --project "$PROJECT" \
        --format='value(monitoringConfig.managedPrometheusConfig.enabled)' 2>/dev/null)" != "True" ]; then
  echo "Enabling Managed Prometheus..."
  gcloud container clusters update "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --enable-managed-prometheus
else
  echo "Managed Prometheus already enabled."
fi

# 2. Enable Hubble + Hubble flow metrics on Cilium (policy/drop with source+FQDN labels).
echo "Enabling Hubble metrics on Cilium..."
cilium upgrade --reuse-values \
  --set hubble.enabled=true \
  --set hubble.relay.enabled=true \
  --set hubble.metrics.enableOpenMetrics=true \
  --set hubble.metrics.enabled="{dns,drop:sourceContext=pod;destinationContext=dns,flow,policy:sourceContext=pod;destinationContext=dns}"
kubectl -n kube-system rollout restart ds/cilium
kubectl -n kube-system rollout status ds/cilium --timeout=180s

# 3. Managed Prometheus collectors that started BEFORE Cilium can be stuck with
#    "no route to host" to the API server; restart them so they pick up Cilium
#    networking. (Harmless if they're already healthy.)
if kubectl -n gmp-system get daemonset/collector >/dev/null 2>&1; then
  kubectl -n gmp-system rollout restart daemonset/collector
  kubectl -n gmp-system rollout status daemonset/collector --timeout=120s
fi

# 4. Scrape Hubble metrics into Managed Prometheus + install the alert rule.
kubectl apply -f "$M/clusterpodmonitoring.yaml"
kubectl apply -f "$M/clusterrules.yaml"

echo
echo "Done. Hubble policy/drop metrics now flow into Managed Prometheus."
echo "Try (PromQL via Cloud Monitoring / Managed Prometheus):"
echo '  sum by (source,destination) (rate(hubble_policy_verdicts_total{action="dropped",direction="egress"}[5m]))'
echo
echo "Query Managed Prometheus directly:"
cat <<EOF
  curl -s -H "Authorization: Bearer \$(gcloud auth application-default print-access-token)" \\
    "https://monitoring.googleapis.com/v1/projects/${PROJECT}/location/global/prometheus/api/v1/query" \\
    --data-urlencode 'query=hubble_policy_verdicts_total{action="dropped",direction="egress"}'
EOF
