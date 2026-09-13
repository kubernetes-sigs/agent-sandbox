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
#
# End-to-end test for the execution-scoped-token example, on kind.
#
# Brings up (or reuses) a kind cluster with Cilium as the CNI — kind's default
# CNI does not enforce NetworkPolicy, which would make the "direct egress is
# blocked" check vacuous — installs the agent-sandbox controller, deploys the
# model gateway and the Sandbox, and runs runner/, which proves four things and
# prints each with the status code and body it actually observed:
#
#   1. during the run, the gateway ACCEPTS the run's token;
#   2. from inside the sandbox, a direct connection to the provider is blocked;
#   3. after the process exits, that SAME token is refused 401 "gateway token
#      revoked";
#   4. a second run in the same Sandbox gets a new run_id and its own working
#      token, while the first one's stays dead.
#
# No provider key is required. Without one the gateway still accepts the token
# and proxies the call; the provider answers with its own authentication error,
# which is distinguishable from the gateway's refusals — see the README's
# "Reading check 1 without a provider key". Set PROVIDER_API_KEY to a real key
# to see a 200 instead.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${SCRIPT_DIR}"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-execution-scoped-token}"
KIND_CONFIG="${KIND_CONFIG:-${SCRIPT_DIR}/kind-config.yaml}"
NAMESPACE="${NAMESPACE:-default}"
SANDBOX_NAME="execution-scoped-token-sandbox"

AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.2}"
CILIUM_VERSION="${CILIUM_VERSION:-1.20.1}"
# Extra `helm install cilium` arguments. Needed on hosts whose capability
# bounding set is reduced (nested containers, some CI runners): Cilium's
# clean-cilium-state init container asks for CAP_SYS_MODULE by name and fails
# with "unable to apply caps: operation not permitted" if the node cannot
# grant it. `--set securityContext.privileged=true` makes Cilium take what the
# node actually has instead of naming capabilities.
CILIUM_HELM_EXTRA_ARGS="${CILIUM_HELM_EXTRA_ARGS:-}"

# The host:port the in-sandbox probe tries to reach DIRECTLY, to show the
# NetworkPolicy drops it. Resolved here, on the host, and passed to the probe as
# an IP so that a DNS failure inside the box can never be mistaken for policy
# enforcement.
PROVIDER_HOST="${PROVIDER_HOST:-api.anthropic.com}"
PROVIDER_PORT="${PROVIDER_PORT:-443}"

# Set to a real key to see check 1 return 200 from the provider. Left as an
# obvious placeholder otherwise: the gateway refuses to start with no provider
# key at all, and the example does not need a working one.
PROVIDER_API_KEY="${PROVIDER_API_KEY:-placeholder-not-a-real-key}"

# Local port-forwards the runner talks to.
GATEWAY_LOCAL_PORT="${GATEWAY_LOCAL_PORT:-18866}"
SANDBOXD_LOCAL_PORT="${SANDBOXD_LOCAL_PORT:-19090}"

# Keep the cluster after a run (handy while iterating).
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"

WORKDIR=""
GATEWAY_PF_PID=""
SANDBOXD_PF_PID=""
CREATED_CLUSTER=false

# --- helpers ----------------------------------------------------------------

log() { printf '\n>>> %s\n' "$*"; }

require() {
  local missing=0
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "ERROR: ${tool} not found on PATH" >&2; missing=1; }
  done
  [ "${missing}" -eq 0 ] || exit 1
}

# `kubectl wait --for=create` needs kubectl >= 1.31; poll instead so this runs
# on older clients too. Args: <label-selector> [timeout-s].
wait_for_pod_created() {
  local selector="$1" timeout="${2:-120}" waited=0
  until [ -n "$(kubectl -n "${NAMESPACE}" get pod --selector="${selector}" -o name 2>/dev/null)" ]; do
    if [ "${waited}" -ge "${timeout}" ]; then
      echo "timed out after ${timeout}s waiting for a pod matching ${selector}" >&2
      return 1
    fi
    sleep 2
    waited=$((waited + 2))
  done
}

wait_for_port() {
  local port="$1" what="$2" waited=0
  until bash -c "exec 3<>/dev/tcp/127.0.0.1/${port}" 2>/dev/null; do
    if [ "${waited}" -ge 60 ]; then
      echo "timed out waiting for the ${what} port-forward on 127.0.0.1:${port}" >&2
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

cleanup() {
  local rc=$?
  log "Cleaning up..."
  set +e
  [ -n "${GATEWAY_PF_PID}" ] && kill "${GATEWAY_PF_PID}" 2>/dev/null
  [ -n "${SANDBOXD_PF_PID}" ] && kill "${SANDBOXD_PF_PID}" 2>/dev/null
  if [ "${CREATED_CLUSTER}" = "true" ] && [ "${KEEP_CLUSTER}" != "true" ]; then
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
  else
    kubectl -n "${NAMESPACE}" delete --ignore-not-found -f networkpolicy.yaml
    kubectl -n "${NAMESPACE}" delete --ignore-not-found -f sandbox.yaml
    kubectl -n "${NAMESPACE}" delete --ignore-not-found -f gateway.yaml
    kubectl -n "${NAMESPACE}" delete --ignore-not-found secret \
      model-gateway-auth model-gateway-provider-keys
  fi
  # The HMAC secret and admin token live here; they are throwaway, but there is
  # no reason to leave credentials on disk after the test.
  [ -n "${WORKDIR}" ] && rm -rf -- "${WORKDIR}"
  exit "${rc}"
}

# --- preflight --------------------------------------------------------------

require kind kubectl helm go getent

log "Resolving ${PROVIDER_HOST} on the host, to probe direct egress by IP"
PROVIDER_IP="$(getent ahostsv4 "${PROVIDER_HOST}" | awk 'NR==1{print $1}')"
if [ -z "${PROVIDER_IP}" ]; then
  echo "ERROR: could not resolve ${PROVIDER_HOST}. This host needs DNS: the gateway" >&2
  echo "       proxies to the real provider, and the egress check needs its address." >&2
  exit 1
fi
echo "${PROVIDER_HOST} -> ${PROVIDER_IP}"

# --- cluster ----------------------------------------------------------------

if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  log "Reusing existing kind cluster '${KIND_CLUSTER_NAME}'"
else
  log "Creating kind cluster '${KIND_CLUSTER_NAME}' (default CNI disabled, for Cilium)"
  kind create cluster --name "${KIND_CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 0s
  CREATED_CLUSTER=true
fi
kubectl config use-context "kind-${KIND_CLUSTER_NAME}" >/dev/null
trap cleanup EXIT

if helm status cilium -n kube-system >/dev/null 2>&1; then
  log "Cilium already installed; skipping"
else
  log "Installing Cilium ${CILIUM_VERSION} — kind's default CNI does not enforce NetworkPolicy"
  helm repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
  helm repo update >/dev/null
  # shellcheck disable=SC2086 # CILIUM_HELM_EXTRA_ARGS is deliberately word-split
  helm install cilium cilium/cilium --version "${CILIUM_VERSION}" \
    --namespace kube-system \
    --set ipam.mode=kubernetes \
    --set operator.replicas=1 \
    ${CILIUM_HELM_EXTRA_ARGS} >/dev/null
fi
kubectl -n kube-system rollout status ds/cilium --timeout=600s
kubectl wait --for=condition=Ready node --all --timeout=300s

if kubectl get crd sandboxes.agents.x-k8s.io >/dev/null 2>&1; then
  log "agent-sandbox already installed; skipping"
else
  log "Installing the agent-sandbox controller ${AGENT_SANDBOX_VERSION}"
  kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox.yaml"
fi
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=300s

# --- credentials ------------------------------------------------------------

log "Generating a throwaway HMAC secret and gateway admin token"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/execution-scoped-token.XXXXXX")"
chmod 700 "${WORKDIR}"
# The HMAC secret is the trust root between the minter (runner/) and the
# verifier (the gateway). It never enters the sandbox: the sandbox only ever
# sees one already-signed, run-scoped token.
head -c 32 /dev/urandom | base64 | tr -d '\n' > "${WORKDIR}/jwt.secret"
head -c 32 /dev/urandom | base64 | tr -d '\n' > "${WORKDIR}/gateway-admin.token"

kubectl -n "${NAMESPACE}" create secret generic model-gateway-auth \
  --from-file=jwt.secret="${WORKDIR}/jwt.secret" \
  --from-file=gateway-admin.token="${WORKDIR}/gateway-admin.token" \
  --dry-run=client -o yaml | kubectl -n "${NAMESPACE}" apply -f -

if [ "${PROVIDER_API_KEY}" = "placeholder-not-a-real-key" ]; then
  echo "NOTE: no PROVIDER_API_KEY set. Check 1 will show the PROVIDER's own"
  echo "      authentication error, which still proves the gateway accepted the"
  echo "      token and proxied the call. See the README."
fi
kubectl -n "${NAMESPACE}" create secret generic model-gateway-provider-keys \
  --from-literal=ANTHROPIC_API_KEY="${PROVIDER_API_KEY}" \
  --dry-run=client -o yaml | kubectl -n "${NAMESPACE}" apply -f -

# --- deploy -----------------------------------------------------------------

log "Deploying the gateway, the Sandbox, and the egress NetworkPolicy"
kubectl -n "${NAMESPACE}" apply -f gateway.yaml -f sandbox.yaml -f networkpolicy.yaml

kubectl -n "${NAMESPACE}" rollout status deploy/model-gateway --timeout=300s
# The controller turns the Sandbox CR into a Pod a moment after the apply;
# `kubectl wait` errors outright if nothing matches yet.
wait_for_pod_created "sandbox=${SANDBOX_NAME}" 120
kubectl -n "${NAMESPACE}" wait --for=condition=ready pod \
  --selector="sandbox=${SANDBOX_NAME}" --timeout=300s

SANDBOX_POD="$(kubectl -n "${NAMESPACE}" get pod --selector="sandbox=${SANDBOX_NAME}" \
  -o jsonpath='{.items[0].metadata.name}')"

log "Confirming the box holds no credentials of its own"
# No service-account token: automountServiceAccountToken: false in sandbox.yaml.
if kubectl -n "${NAMESPACE}" exec "${SANDBOX_POD}" -- \
    cat /var/run/secrets/kubernetes.io/serviceaccount/token >/dev/null 2>&1; then
  echo "FAIL: a service-account token is mounted in the sandbox" >&2
  exit 1
fi
echo "OK: no service-account token in the sandbox."
# And no gateway token in the pod spec — the assertion that the token really
# is not going to arrive the way most examples pass credentials.
if kubectl -n "${NAMESPACE}" get pod "${SANDBOX_POD}" -o yaml | grep -qi 'EST_GATEWAY_TOKEN'; then
  echo "FAIL: a gateway token is present in the pod spec" >&2
  exit 1
fi
echo "OK: no gateway token in the pod spec (it arrives per-process, via ProcessConfig.env_vars)."

# --- port-forwards ----------------------------------------------------------

log "Port-forwarding the gateway (:${GATEWAY_LOCAL_PORT}) and sandboxd (:${SANDBOXD_LOCAL_PORT})"
kubectl -n "${NAMESPACE}" port-forward "svc/model-gateway" \
  "${GATEWAY_LOCAL_PORT}:8866" >/dev/null 2>&1 &
GATEWAY_PF_PID=$!
kubectl -n "${NAMESPACE}" port-forward "pod/${SANDBOX_POD}" \
  "${SANDBOXD_LOCAL_PORT}:9090" >/dev/null 2>&1 &
SANDBOXD_PF_PID=$!
wait_for_port "${GATEWAY_LOCAL_PORT}" gateway
wait_for_port "${SANDBOXD_LOCAL_PORT}" sandboxd

# --- the actual test --------------------------------------------------------

log "go vet ./runner"
(cd "${REPO_ROOT}" && go vet ./examples/containarium-execution-scoped-token/runner)

log "Running the runner: two runs in the same Sandbox, one credential each"
(cd "${REPO_ROOT}" && go run ./examples/containarium-execution-scoped-token/runner \
  -sandboxd-addr "127.0.0.1:${SANDBOXD_LOCAL_PORT}" \
  -gateway-url "http://127.0.0.1:${GATEWAY_LOCAL_PORT}" \
  -gateway-in-cluster-host "model-gateway.${NAMESPACE}.svc.cluster.local" \
  -gateway-in-cluster-port 8866 \
  -secret-file "${WORKDIR}/jwt.secret" \
  -admin-token-file "${WORKDIR}/gateway-admin.token" \
  -provider-addr "${PROVIDER_IP}:${PROVIDER_PORT}" \
  -provider-host "${PROVIDER_HOST}")

log "Gateway log (the revoked-token refusals it recorded)"
kubectl -n "${NAMESPACE}" logs deploy/model-gateway --tail=20

log "Test finished."
