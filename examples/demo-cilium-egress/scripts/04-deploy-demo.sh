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

# Deploy the demo: namespace + identities, baseline DNS policy, team-a github allow,
# and the two sandboxes. (Phase 2 / team-b allow is applied by test.sh.)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"

kubectl apply -f "$M/00-namespace-and-identities.yaml"
kubectl apply -f "$M/10-cilium-allow-dns.yaml"
kubectl apply -f "$M/20-cilium-team-a-allow-github.yaml"
# Ensure phase-1 state on re-runs: remove any team-b allow left over from phase 2.
kubectl delete -f "$M/40-cilium-team-b-allow-github.yaml" --ignore-not-found
kubectl apply -f "$M/30-sandboxes.yaml"

echo "Waiting for sandboxes to be Ready..."
kubectl wait --for=condition=Ready sandbox/sandbox-team-a sandbox/sandbox-team-b \
  -n "$NAMESPACE" --timeout=180s

kubectl get sandbox -n "$NAMESPACE"
kubectl get pods -n "$NAMESPACE" -L demo.identity
echo "OK: demo deployed (phase 1 — team-a allowed, team-b denied)."
