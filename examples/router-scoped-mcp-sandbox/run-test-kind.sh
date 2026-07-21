#!/bin/bash
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
#
# Smoke test: deploy sandbox-router with --authz-mode=scoped-token, create
# two Sandboxes (box-a, box-b), and verify:
#   1. A token minted for box-a lets the MCP client reach box-a — with no
#      kubeconfig, kubectl, or ssh involved on the client side at all.
#   2. The SAME token is rejected (403) against box-b — the per-sandbox
#      scoping property TokenReviewAuthorizer does not provide.
#   3. A malformed/forged token is rejected (401) against box-a.
#
# Assumes a cluster with the Agent Sandbox controller + CRDs already
# installed (see ../../README.md#installation) — same prerequisite as
# mcp-server-sandbox and containarium-ssh-sandbox. This script builds and
# deploys sandbox-router itself, since the scoped-token authorizer isn't
# in any published router image yet.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXAMPLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROUTER_IMAGE="${ROUTER_IMAGE:-kind.local/sandbox-router-go:scoped-token-example}"
MCP_IMAGE="${MCP_IMAGE:-kind.local/router-scoped-mcp-sandbox:example}"
KIND_CLUSTER="${KIND_CLUSTER:-agent-sandbox}"

wait_for_pod_created() {
  local selector="$1" timeout="${2:-60}" waited=0
  until [ -n "$(kubectl get pod --selector="${selector}" -o name 2>/dev/null)" ]; do
    if [ "${waited}" -ge "${timeout}" ]; then
      echo "timed out after ${timeout}s waiting for a pod matching ${selector}" >&2
      return 1
    fi
    sleep 2
    waited=$((waited + 2))
  done
}

echo "Building sandbox-router image (${ROUTER_IMAGE})..."
docker build -f "${REPO_ROOT}/sandbox-router/Dockerfile" -t "${ROUTER_IMAGE}" "${REPO_ROOT}"
kind load docker-image "${ROUTER_IMAGE}" --name "${KIND_CLUSTER}"

echo "Building MCP server image (${MCP_IMAGE})..."
docker build -t "${MCP_IMAGE}" "${EXAMPLE_DIR}"
kind load docker-image "${MCP_IMAGE}" --name "${KIND_CLUSTER}"

echo "Generating the shared scoped-token secret..."
SECRET_FILE="$(mktemp)"
openssl rand -hex 32 > "${SECRET_FILE}"
kubectl create secret generic scoped-token-secret \
  --from-file=secret="${SECRET_FILE}" \
  --dry-run=client -o yaml | kubectl apply -f -

cleanup() {
  echo "Cleaning up..."
  [ -n "${PF_PID:-}" ] && kill "${PF_PID}" 2>/dev/null || true
  kubectl delete --ignore-not-found -f "${EXAMPLE_DIR}/sandbox.yaml"
  kubectl delete --ignore-not-found -f "${EXAMPLE_DIR}/sandbox-box-b.yaml"
  kubectl delete --ignore-not-found secret scoped-token-secret
  kubectl delete --ignore-not-found deploy sandbox-router
  kubectl delete --ignore-not-found svc sandbox-router-svc
  kubectl delete --ignore-not-found -f "${REPO_ROOT}/sandbox-router/deploy/rbac.yaml"
  kubectl delete --ignore-not-found serviceaccount sandbox-router
  rm -f "${SECRET_FILE}"
  if [ -n "${VENV_DIR:-}" ]; then rm -rf "${VENV_DIR}"; fi
}
trap cleanup EXIT

echo "Deploying sandbox-router with --authz-mode=scoped-token..."
kubectl apply -f "${REPO_ROOT}/sandbox-router/deploy/serviceaccount.yaml"
# rbac.yaml grants the pod list/watch the router's pod-IP cache needs;
# without it the cache never syncs and /readyz never goes ready.
kubectl apply -f "${REPO_ROOT}/sandbox-router/deploy/rbac.yaml"
kubectl apply -f "${REPO_ROOT}/sandbox-router/deploy/service.yaml"
sed -e "s|registry.k8s.io/agent-sandbox/sandbox-router-go:latest|${ROUTER_IMAGE}|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    "${REPO_ROOT}/sandbox-router/deploy/deployment.yaml" \
  | kubectl apply -f -

# Append the scoped-token flags (args already exists as an array in the
# base manifest, so a JSON-patch append is safe) and mount the secret
# (volumes/volumeMounts don't exist yet in the base manifest, so this half
# uses a strategic-merge patch, which knows how to add a new named volume
# without needing the key to pre-exist).
kubectl patch deploy sandbox-router --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--authz-mode=scoped-token"},
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--authz-scoped-token-secret-file=/etc/scoped-token/secret"}
]'
kubectl patch deploy sandbox-router --type=strategic -p='{
  "spec": {"template": {"spec": {
    "volumes": [{"name": "scoped-token-secret", "secret": {"secretName": "scoped-token-secret"}}],
    "containers": [{"name": "sandbox-router", "volumeMounts": [
      {"name": "scoped-token-secret", "mountPath": "/etc/scoped-token", "readOnly": true}
    ]}]
  }}}
}'
kubectl rollout status deploy/sandbox-router --timeout=120s

# The client runs on the host, where the in-cluster Service DNS name is
# not resolvable (kind, most local clusters) — port-forward the router
# Service to localhost instead. client.py's default --router-url matches.
echo "Port-forwarding the router Service to localhost:18080..."
kubectl port-forward svc/sandbox-router-svc 18080:8080 &
PF_PID=$!
sleep 3

echo "Applying box-a and box-b Sandboxes..."
export IMAGE="${MCP_IMAGE}"
envsubst < "${EXAMPLE_DIR}/sandbox.yaml" | kubectl apply -f -
envsubst < "${EXAMPLE_DIR}/sandbox-box-b.yaml" | kubectl apply -f -

echo "Waiting for both sandbox pods to be ready..."
wait_for_pod_created sandbox=box-a 60
wait_for_pod_created sandbox=box-b 60
kubectl wait --for=condition=ready pod --selector=sandbox=box-a --timeout=180s
kubectl wait --for=condition=ready pod --selector=sandbox=box-b --timeout=180s

echo "Setting up the Python client environment..."
VENV_DIR="$(mktemp -d)"
python3 -m venv "${VENV_DIR}"
"${VENV_DIR}/bin/pip" install -q -r "${EXAMPLE_DIR}/requirements.txt"

echo "Minting a token scoped to box-a..."
TOKEN_A=$(cd "${REPO_ROOT}" && go run ./examples/router-scoped-mcp-sandbox/mint-token \
  --secret-file="${SECRET_FILE}" --namespace=default --name=box-a --ttl=10m)

echo
echo "=== Test 1: box-a's token against box-a — expect success ==="
"${VENV_DIR}/bin/python3" "${EXAMPLE_DIR}/client.py" \
  --token "${TOKEN_A}" --target-id box-a

echo
echo "=== Test 2: box-a's token against box-b — expect exactly 403 (scoping) ==="
"${VENV_DIR}/bin/python3" "${EXAMPLE_DIR}/client.py" \
  --token "${TOKEN_A}" --target-id box-b --expect-status 403

echo
echo "=== Test 3: a forged token against box-a — expect exactly 401 ==="
"${VENV_DIR}/bin/python3" "${EXAMPLE_DIR}/client.py" \
  --token "not-a-real-token" --target-id box-a --expect-status 401

echo
echo "All checks passed."
