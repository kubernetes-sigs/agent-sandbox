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

set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox}"
NODE_PORT="30789"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Derive the image tag from the template so the two never drift.
# `|| true` swallows the pipeline exit status so `set -euo pipefail` doesn't
# abort before the friendly error check below can fire (e.g., if grep matches
# nothing because the template's `image:` line moved).
IMAGE="$(grep -E '^\s+image:' openclaw-template.yaml | head -1 | awk '{print $2}' || true)"
if [ -z "${IMAGE}" ]; then
  echo "ERROR: could not read image tag from openclaw-template.yaml" >&2
  exit 1
fi

# --- Prechecks --------------------------------------------------------------

if ! kubectl get runtimeclass gvisor >/dev/null 2>&1; then
  cat <<EOF >&2
ERROR: RuntimeClass 'gvisor' not found in the cluster.

This example requires gVisor. Install runsc on the kind node image and create
the RuntimeClass before running this script. See:
  https://gvisor.dev/docs/user_guide/quick_start/kubernetes/
EOF
  exit 1
fi

# --- Image load -------------------------------------------------------------

echo "Pulling ${IMAGE}..."
docker pull "${IMAGE}"

echo "Loading ${IMAGE} into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image "${IMAGE}" --name "${KIND_CLUSTER_NAME}"

# --- Apply ------------------------------------------------------------------

echo "Generating gateway token..."
TOKEN="$(openssl rand -hex 32)"

cleanup() {
  echo "Cleaning up..."
  kubectl delete --ignore-not-found -f kind-service.yaml
  kubectl delete --ignore-not-found -f openclaw-claim.yaml
  kubectl delete --ignore-not-found -f openclaw-warmpool.yaml
  kubectl delete --ignore-not-found -f openclaw-template.yaml
  kubectl delete --ignore-not-found -f openclaw-config.yaml
}
trap cleanup EXIT

echo "Applying manifests..."
kubectl apply -f openclaw-config.yaml
sed "s/dummy-token-for-sandbox/${TOKEN}/g" openclaw-template.yaml | kubectl apply -f -
kubectl apply -f openclaw-warmpool.yaml
kubectl apply -f openclaw-claim.yaml
kubectl apply -f kind-service.yaml

# --- Wait for the claim's pod ----------------------------------------------

echo "Waiting for SandboxClaim to be satisfied..."
for i in $(seq 1 60); do
  SANDBOX_NAME="$(kubectl get sandboxclaim openclaw-sandbox-claim -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"
  [ -n "${SANDBOX_NAME}" ] && break
  sleep 1
done
if [ -z "${SANDBOX_NAME:-}" ]; then
  echo "ERROR: SandboxClaim 'openclaw-sandbox-claim' was never satisfied." >&2
  exit 1
fi

# The agents.x-k8s.io/pod-name annotation is only set when the claim adopts
# a warm-pool sandbox. For cold-started sandboxes the pod name matches the
# Sandbox name itself, so fall back to that if the annotation never appears.
echo "Resolving backing pod (annotation if warm-adopted, else Sandbox name)..."
for i in $(seq 1 30); do
  POD="$(kubectl get sandbox "${SANDBOX_NAME}" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}' 2>/dev/null || true)"
  [ -n "${POD}" ] && break
  sleep 1
done
if [ -z "${POD:-}" ]; then
  POD="${SANDBOX_NAME}"
  echo "Pod-name annotation absent; using Sandbox name as pod name (cold-start path)."
fi
echo "Claimed pod: ${POD}"

echo "Waiting for claimed pod to be ready..."
kubectl wait --for=condition=ready pod/"${POD}" --timeout=180s

# --- Gateway reachability via NodePort -------------------------------------

echo "Checking gateway via NodePort localhost:${NODE_PORT}..."
GATEWAY_READY=false
for i in $(seq 1 30); do
  if curl -sf -o /dev/null "http://127.0.0.1:${NODE_PORT}/"; then
    GATEWAY_READY=true
    break
  fi
  sleep 1
done

if [ "${GATEWAY_READY}" = false ]; then
  cat <<EOF >&2
ERROR: NodePort ${NODE_PORT} is not reachable on localhost.

The kind cluster must be created with an extraPortMappings entry for
${NODE_PORT}. Recreate the cluster with the snippet from README.md.
EOF
  exit 1
fi
echo "Gateway responded on NodePort ${NODE_PORT}."

# --- PVC persistence test ---------------------------------------------------

CANARY="persistence-canary-$(openssl rand -hex 4)"
echo "Writing canary to /workspace/.openclaw/canary.txt in ${POD}..."
kubectl exec "${POD}" -- sh -c "echo '${CANARY}' > /workspace/.openclaw/canary.txt"

EXPECTED="$(kubectl exec "${POD}" -- cat /workspace/.openclaw/canary.txt)"
if [ "${EXPECTED}" != "${CANARY}" ]; then
  echo "ERROR: canary write/read mismatch in original pod." >&2
  exit 1
fi

OLD_UID="$(kubectl get pod "${POD}" -o jsonpath='{.metadata.uid}')"
echo "Deleting pod ${POD} (UID: ${OLD_UID}) to force a respawn..."
kubectl delete pod "${POD}" --wait=true

echo "Waiting for the Sandbox controller to respawn the pod..."
for i in $(seq 1 60); do
  CURRENT_UID="$(kubectl get pod "${POD}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  [ -n "${CURRENT_UID}" ] && [ "${CURRENT_UID}" != "${OLD_UID}" ] && break
  sleep 2
done
if [ -z "${CURRENT_UID:-}" ] || [ "${CURRENT_UID}" = "${OLD_UID}" ]; then
  echo "ERROR: replacement pod never appeared." >&2
  exit 1
fi
echo "Replacement pod has appeared (new UID: ${CURRENT_UID})."
kubectl wait --for=condition=ready pod/"${POD}" --timeout=180s

ACTUAL="$(kubectl exec "${POD}" -- cat /workspace/.openclaw/canary.txt)"
if [ "${ACTUAL}" != "${CANARY}" ]; then
  echo "FAIL: PVC did not persist across pod respawn." >&2
  echo "  expected: ${CANARY}" >&2
  echo "  actual:   ${ACTUAL}" >&2
  exit 1
fi

echo "PASS: PVC persisted across pod respawn (${CANARY})."
echo "Test finished."
