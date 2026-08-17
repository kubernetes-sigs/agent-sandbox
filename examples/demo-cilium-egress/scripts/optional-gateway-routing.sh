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

# OPTIONAL ingress add-on: stand up the agent-sandbox "gateway routing" path
# (GKE Gateway API -> sandbox-router -> sandbox pod) and switch the two demo
# sandboxes to an HTTP exec-server image so they can be driven over HTTP.
#
# After this, you can drive `git clone` THROUGH the gateway and watch the same
# per-identity Cilium egress policy allow team-a and deny team-b.
#
# Images are built with Cloud Build (no local docker/buildx needed, amd64-native).
# Requires: gcloud (Cloud Build API enabled) and kubectl. Enables the GKE Gateway
# API controller on the cluster if it isn't already.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"

REPO_ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
DEMO="$REPO_ROOT/examples/demo-cilium-egress"
ROUTER_SRC="$REPO_ROOT/clients/python/agentic-sandbox-client/sandbox-router"

AR_LOC="${AR_LOC:-us-central1}"
AR_REPO="${AR_REPO:-agent-sandbox-demo}"
AR="${AR_LOC}-docker.pkg.dev/${PROJECT}/${AR_REPO}"
ROUTER_IMAGE="${ROUTER_IMAGE:-$AR/sandbox-router:latest}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-$AR/exec-sandbox:latest}"

# Router auth is on by default: we mint a random Bearer token into the
# sandbox-router-auth Secret and the router requires it on /execute. To
# deliberately expose an UNAUTHENTICATED /execute (public RCE) on a throwaway
# cluster, run with ALLOW_UNAUTHENTICATED_ROUTER=true.
ALLOW_UNAUTHENTICATED_ROUTER="${ALLOW_UNAUTHENTICATED_ROUTER:-false}"

# 0. Ensure the GKE Gateway API controller is enabled on the cluster.
if ! kubectl get gatewayclass gke-l7-global-external-managed >/dev/null 2>&1; then
  echo "Enabling GKE Gateway API on the cluster (one-time, a few minutes)..."
  gcloud container clusters update "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --gateway-api=standard
fi

# 1. Artifact Registry repo + build both images with Cloud Build.
if ! gcloud artifacts repositories describe "$AR_REPO" --location "$AR_LOC" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud artifacts repositories create "$AR_REPO" --repository-format=docker --location "$AR_LOC" --project "$PROJECT"
fi
echo "Building images with Cloud Build..."
gcloud builds submit --project "$PROJECT" --tag "$ROUTER_IMAGE" "$ROUTER_SRC"
gcloud builds submit --project "$PROJECT" --tag "$SANDBOX_IMAGE" "$DEMO/exec-sandbox"

# 2. Switch the sandboxes to the HTTP exec image (same identities/policies).
#    Delete first: agent-sandbox does not recreate the pod on an in-place image
#    change (see kubernetes-sigs/agent-sandbox#581), so we replace the Sandboxes.
kubectl delete sandbox sandbox-team-a sandbox-team-b -n "$NAMESPACE" --ignore-not-found
# Wait for deletion to finish so the re-apply doesn't race a terminating object.
kubectl wait --for=delete sandbox/sandbox-team-a sandbox/sandbox-team-b -n "$NAMESPACE" --timeout=180s 2>/dev/null || true
tmp="$(mktemp)"
sed "s#__SANDBOX_IMAGE__#${SANDBOX_IMAGE}#g" "$DEMO/manifests/ingress/30-sandboxes-http.yaml" > "$tmp"
kubectl apply -f "$tmp"; rm -f "$tmp"
kubectl wait --for=condition=Ready sandbox/sandbox-team-a sandbox/sandbox-team-b -n "$NAMESPACE" --timeout=180s

# 3. Router auth. Secure by default: mint a token Secret the router enforces.
if [ "$ALLOW_UNAUTHENTICATED_ROUTER" = "true" ]; then
  echo "WARNING: deploying sandbox-router with NO auth. With the public Gateway this"
  echo "         exposes /execute to the internet (unauthenticated RCE). Throwaway clusters only."
  kubectl delete secret sandbox-router-auth -n "$NAMESPACE" --ignore-not-found
else
  ROUTER_AUTH_TOKEN="${ROUTER_AUTH_TOKEN:-$(openssl rand -hex 24)}"
  kubectl create secret generic sandbox-router-auth -n "$NAMESPACE" \
    --from-literal=auth-token="$ROUTER_AUTH_TOKEN" \
    --dry-run=client -o yaml | kubectl apply -f -
  echo "Router auth enabled; /execute requires a Bearer token (stored in Secret sandbox-router-auth)."
fi

# 4. Deploy router + GKE Gateway.
tmp="$(mktemp)"
sed -e "s#__ROUTER_IMAGE__#${ROUTER_IMAGE}#g" \
    -e "s#__ALLOW_UNAUTHENTICATED_ROUTER__#${ALLOW_UNAUTHENTICATED_ROUTER}#g" \
    "$DEMO/manifests/router/sandbox-router.yaml" > "$tmp"
kubectl apply -f "$tmp"; rm -f "$tmp"
kubectl apply -f "$DEMO/manifests/router/gateway.yaml"
kubectl -n "$NAMESPACE" rollout status deploy/sandbox-router-deployment --timeout=180s

# 5. Wait for the Gateway to get an external address (can take several minutes).
echo "Waiting for the Gateway external address..."
ip=""
for _ in $(seq 1 40); do
  ip="$(kubectl -n "$NAMESPACE" get gateway external-http-gateway -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
  [ -n "$ip" ] && break
  sleep 20
done
echo "Gateway address: ${ip:-<pending>}"
[ -n "$ip" ] || { echo "ERROR: Gateway did not receive an external address within the timeout." >&2; exit 1; }
echo
echo "Ingress is up. Verify end-to-end with:  ./scripts/test-ingress.sh"
