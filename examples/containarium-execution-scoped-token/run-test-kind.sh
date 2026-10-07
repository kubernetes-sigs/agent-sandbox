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
# Labeled onto every resource this run creates, and the only thing cleanup()
# deletes on the reuse path — never a fixed name — so a stale or concurrent
# run's resources can never be mistaken for this run's own. Set here, before
# `trap cleanup EXIT` below, so it is always defined if cleanup runs early.
RUN_ID="run-$(date +%s)-$$"
RUN_LABEL="execution-scoped-token/run-id=${RUN_ID}"

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

# Rejects a malformed *_PORT override before it reaches anywhere that builds
# a command from it. Not only for the injection surface — every bash -c
# below passes its port as a positional argument, not interpolated into
# shell source, so a bad value can't execute anything either way — but
# because a non-numeric or out-of-range port otherwise fails confusingly
# deep inside a /dev/tcp probe instead of here, with a clear reason.
require_port() {
  local name="$1" value="$2"
  case "${value}" in
    ''|*[!0-9]*)
      echo "ERROR: ${name}='${value}' is not a valid port (must be an integer 1-65535)" >&2
      exit 1
      ;;
  esac
  if [ "${value}" -lt 1 ] || [ "${value}" -gt 65535 ]; then
    echo "ERROR: ${name}=${value} is out of range (must be 1-65535)" >&2
    exit 1
  fi
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
  until bash -c 'exec 3<>"/dev/tcp/127.0.0.1/$1"' _ "${port}" 2>/dev/null; do
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
    # By RUN_ID label, not by fixed name: a fixed-name delete would remove
    # another invocation's resources if one is running concurrently in the
    # same namespace. Matching nothing (e.g. the run never got past the
    # collision check) is a harmless no-op.
    kubectl -n "${NAMESPACE}" delete --ignore-not-found \
      -l "${RUN_LABEL}" \
      networkpolicy,deployment,service,secret,sandboxes.agents.x-k8s.io
  fi
  # The HMAC secret and admin token live here; they are throwaway, but there is
  # no reason to leave credentials on disk after the test.
  [ -n "${WORKDIR}" ] && rm -rf -- "${WORKDIR}"
  exit "${rc}"
}

# The tag sandbox.yaml's sandboxd container references. Never pulled from a
# registry (imagePullPolicy: Never in sandbox.yaml) — built and `kind load`ed
# below, every run, so the image can't drift from the source tree under test.
SANDBOXD_LOCAL_IMAGE="sandboxd:execution-scoped-token-local"

# --- preflight --------------------------------------------------------------

require kind kubectl helm go getent docker timeout
require_port PROVIDER_PORT "${PROVIDER_PORT}"
require_port GATEWAY_LOCAL_PORT "${GATEWAY_LOCAL_PORT}"
require_port SANDBOXD_LOCAL_PORT "${SANDBOXD_LOCAL_PORT}"

log "Resolving ${PROVIDER_HOST} on the host, to probe direct egress by IP"
PROVIDER_IP="$(getent ahostsv4 "${PROVIDER_HOST}" | awk 'NR==1{print $1}')"
if [ -z "${PROVIDER_IP}" ]; then
  echo "ERROR: could not resolve ${PROVIDER_HOST}. This host needs DNS: the gateway" >&2
  echo "       proxies to the real provider, and the egress check needs its address." >&2
  exit 1
fi
echo "${PROVIDER_HOST} -> ${PROVIDER_IP}"

# Check 2 (inside the sandbox, later) proves the SANDBOX cannot reach
# ${PROVIDER_IP}:${PROVIDER_PORT} directly. That is only evidence of
# NetworkPolicy enforcement if something the policy does NOT select can
# reach the same address — otherwise a bad IP, a firewalled destination, or
# an unrelated outage would make check 2 pass for the wrong reason, exactly
# as vacuously as an unenforced policy would (see networkpolicy.yaml's note
# on kindnet). This host is that unselected control: it is not a pod the
# NetworkPolicy's podSelector can ever match, so if IT cannot reach the
# address either, the sandbox failing to reach it proves nothing and the
# run stops here rather than reporting a false pass later. Same `/dev/tcp`
# mechanism runner/probe.go uses inside the sandbox, so a "reachable" here
# and a "blocked" there are directly comparable.
log "Confirming ${PROVIDER_IP}:${PROVIDER_PORT} is reachable from here (the unselected control for check 2)"
if ! timeout 10 bash -c "exec 3<>\"/dev/tcp/${PROVIDER_IP}/\$1\"" _ "${PROVIDER_PORT}" 2>/dev/null; then
  echo "ERROR: this host could not reach ${PROVIDER_IP}:${PROVIDER_PORT}" >&2
  echo "       (resolved from ${PROVIDER_HOST}). Check 2 later would show the" >&2
  echo "       sandbox blocked from the same address, but that would not prove" >&2
  echo "       NetworkPolicy enforcement — it would just mean the address is" >&2
  echo "       unreachable from here too. Fix connectivity to ${PROVIDER_HOST}," >&2
  echo "       or set PROVIDER_HOST/PROVIDER_PORT to a reachable endpoint," >&2
  echo "       before re-running." >&2
  exit 1
fi
echo "OK: this host reached ${PROVIDER_IP}:${PROVIDER_PORT} — check 2's block will be meaningful."

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

if [ "${CREATED_CLUSTER}" != "true" ]; then
  # A freshly created cluster is guaranteed correct by construction —
  # kind-config.yaml's disableDefaultCNI: true, applied just above. A REUSED
  # cluster is not: if it predates this script, or was created without that
  # config, kind's default CNI (kindnet) is still running. kindnet does not
  # enforce NetworkPolicy (see networkpolicy.yaml's note on this), so
  # installing Cilium alongside it does not make the policy enforced — it
  # leaves two CNIs on the same pods, and would make the egress check below
  # pass or fail for reasons that have nothing to do with the policy.
  if kubectl -n kube-system get daemonset kindnet >/dev/null 2>&1; then
    echo "ERROR: reused cluster '${KIND_CLUSTER_NAME}' still runs kindnet (a" >&2
    echo "       kube-system DaemonSet named 'kindnet'), so it was not created" >&2
    echo "       with this example's kind-config.yaml (disableDefaultCNI: true)." >&2
    echo "       kindnet does not enforce NetworkPolicy, so the egress check" >&2
    echo "       below would not be meaningful even with Cilium also installed." >&2
    echo "       Delete it and let this script recreate it correctly:" >&2
    echo "         kind delete cluster --name ${KIND_CLUSTER_NAME}" >&2
    exit 1
  fi
fi

# --- resource collision check ------------------------------------------------

# A reused cluster (above) can still carry live resources from a DIFFERENT,
# still-running invocation of this same script, or stale ones a crashed prior
# run's `trap cleanup EXIT` never got to remove. Reusing those fixed names
# here would let this run's own cleanup() delete resources it did not
# create, or let this run's apply silently take over a Sandbox/gateway a
# concurrent run is still mid-test with.
#
# The lock below is what actually closes this: `kubectl create` (never
# `apply`) on a fixed-name resource is atomic at the API server — two
# invocations racing to create the same name can never both succeed, so
# there is no window between "checked, looked free" and "claimed it" for a
# second invocation to land in. Everything else this run creates is labeled
# with RUN_ID and only claimed once this lock is held.
log "Claiming namespace '${NAMESPACE}' on cluster '${KIND_CLUSTER_NAME}' for this run"
if ! kubectl -n "${NAMESPACE}" create secret generic execution-scoped-token-lock \
    --from-literal=run_id="${RUN_ID}" >/dev/null 2>&1; then
  echo "ERROR: could not claim namespace '${NAMESPACE}' on cluster" >&2
  echo "       '${KIND_CLUSTER_NAME}' — a secret named" >&2
  echo "       'execution-scoped-token-lock' already exists there." >&2
  echo "       This looks like a still-running or crashed prior invocation of" >&2
  echo "       this script. Wait for that run to finish, delete its leftovers" >&2
  echo "       by hand (including that lock secret), or set NAMESPACE to an" >&2
  echo "       unused one." >&2
  exit 1
fi
kubectl -n "${NAMESPACE}" label --overwrite secret/execution-scoped-token-lock "${RUN_LABEL}" >/dev/null

# Best-effort, NOT what makes this race-free (the lock above is) — this only
# catches leftovers from a script version older than the lock itself, which
# would never have created one to collide on.
COLLIDING=""
for r in \
  "secret/model-gateway-auth" \
  "secret/model-gateway-provider-keys" \
  "deployment/model-gateway" \
  "service/model-gateway" \
  "sandboxes.agents.x-k8s.io/${SANDBOX_NAME}" \
  "networkpolicy/execution-scoped-token-sandbox-egress"
do
  if kubectl -n "${NAMESPACE}" get "${r}" >/dev/null 2>&1; then
    COLLIDING="${COLLIDING} ${r}"
  fi
done
if [ -n "${COLLIDING}" ]; then
  echo "ERROR: namespace '${NAMESPACE}' on cluster '${KIND_CLUSTER_NAME}' already has:" >&2
  echo "       ${COLLIDING}" >&2
  echo "       These predate this script's lock secret (above), so they were" >&2
  echo "       not caught by it — likely leftovers from an older run. Delete" >&2
  echo "       them by hand, or set NAMESPACE to an unused one." >&2
  exit 1
fi
log "No pre-existing resources under this run's names in '${NAMESPACE}' — proceeding as ${RUN_ID}"

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

# --- in-cluster reachability control -----------------------------------------

# The host-side check above rules out "the address is unreachable, period."
# It does not rule out a difference between the host's network path and a
# POD's — a node-level route, NAT rule, or CNI quirk that only affects pod-
# sourced traffic. This is the stronger, cluster-native control: a throwaway
# pod in this namespace, deliberately NOT labeled `sandbox: ...`, so the
# NetworkPolicy's podSelector can never match it — the same "unselected" as
# a hostile or unrelated workload sharing this namespace would be, and
# exactly what the sandbox's own PASS/FAIL on this same address is compared
# against. If THIS can't reach it either, the sandbox failing to reach it
# proves nothing about the policy specifically.
log "Confirming ${PROVIDER_IP}:${PROVIDER_PORT} is ALSO reachable from an unselected in-cluster pod"
if ! kubectl -n "${NAMESPACE}" run execution-scoped-token-egress-control \
    --image=busybox:1.36 --restart=Never --rm -i --quiet \
    --labels="${RUN_LABEL}" \
    --command -- nc -z -w 5 "${PROVIDER_IP}" "${PROVIDER_PORT}"; then
  echo "ERROR: an unselected in-cluster pod (no 'sandbox' label — the" >&2
  echo "       NetworkPolicy cannot apply to it) could NOT reach" >&2
  echo "       ${PROVIDER_IP}:${PROVIDER_PORT}. Check 2 later would show the" >&2
  echo "       sandbox blocked from the same address, but that would not" >&2
  echo "       prove NetworkPolicy enforcement specifically — it would mean" >&2
  echo "       the address is unreachable from this cluster's pod network" >&2
  echo "       too. Fix connectivity to ${PROVIDER_HOST} from inside the" >&2
  echo "       cluster, or set PROVIDER_HOST/PROVIDER_PORT to a reachable" >&2
  echo "       endpoint, before re-running." >&2
  exit 1
fi
echo "OK: an unselected in-cluster pod also reached ${PROVIDER_IP}:${PROVIDER_PORT} — check 2's block will be meaningful at the pod-network level, not just from this host."

# --- sandboxd image ----------------------------------------------------------

# sandboxd has no released image yet (README: "Why sandboxd is built
# locally"). The only published builds sit on the k8s staging registry, which
# is periodically garbage-collected — a digest pin there still eventually
# 404s, so this test does not depend on it. Build the daemon from
# packages/sandboxd/Dockerfile instead and load it straight into kind;
# sandbox.yaml references this exact tag with imagePullPolicy: Never.
log "Building ${SANDBOXD_LOCAL_IMAGE} from packages/sandboxd/Dockerfile"
docker build -f "${REPO_ROOT}/packages/sandboxd/Dockerfile" -t "${SANDBOXD_LOCAL_IMAGE}" "${REPO_ROOT}"
log "Loading ${SANDBOXD_LOCAL_IMAGE} into kind cluster '${KIND_CLUSTER_NAME}'"
kind load docker-image "${SANDBOXD_LOCAL_IMAGE}" --name "${KIND_CLUSTER_NAME}"

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

# Labeled with this run's RUN_ID so cleanup() below deletes only what this
# run created — see "resource collision check" above for why that matters.
kubectl -n "${NAMESPACE}" label --overwrite \
  secret/model-gateway-auth secret/model-gateway-provider-keys \
  "${RUN_LABEL}" >/dev/null

# --- deploy -----------------------------------------------------------------

log "Deploying the gateway, the Sandbox, and the egress NetworkPolicy"
kubectl -n "${NAMESPACE}" apply -f gateway.yaml -f sandbox.yaml -f networkpolicy.yaml
kubectl -n "${NAMESPACE}" label --overwrite \
  deployment/model-gateway service/model-gateway \
  "sandboxes.agents.x-k8s.io/${SANDBOX_NAME}" \
  networkpolicy/execution-scoped-token-sandbox-egress \
  "${RUN_LABEL}" >/dev/null

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
#
# `kubectl exec ... -- cat <path>` returns non-zero both when the path is
# genuinely absent (cat's own "No such file or directory") AND when kubectl
# itself never got to run anything — pod not found, RBAC denial, a transport
# error. Those two are opposite outcomes: the first is what this check wants
# to see, the second means the check never actually ran. Collapsing them (as
# an earlier version of this script did) reports "OK, verified secure" on an
# API error just as readily as on a real negative result. `-- true` first
# proves the exec channel itself works, so a later non-zero from the real
# command can be trusted as "file absent" rather than "couldn't check."
if ! kubectl -n "${NAMESPACE}" exec "${SANDBOX_POD}" -- true >/dev/null 2>&1; then
  echo "ERROR: kubectl exec into ${SANDBOX_POD} failed, so whether a" >&2
  echo "       service-account token is mounted could not be verified — this" >&2
  echo "       is an inconclusive check, not a confirmed pass." >&2
  exit 1
fi
if kubectl -n "${NAMESPACE}" exec "${SANDBOX_POD}" -- \
    cat /var/run/secrets/kubernetes.io/serviceaccount/token >/dev/null 2>&1; then
  echo "FAIL: a service-account token is mounted in the sandbox" >&2
  exit 1
fi
echo "OK: no service-account token in the sandbox."

# And no gateway token in the pod spec — the assertion that the token really
# is not going to arrive the way most examples pass credentials.
#
# Piping straight into `grep -qi` has the same fail-open shape as above: with
# `pipefail` set, a pipeline's exit code is the rightmost command that
# failed, and `grep -q` with no match exits 1 on its OWN — the exact code a
# failed `kubectl get` also produces. A pipefail failure here is therefore
# indistinguishable from the good case (no token present), so a broken
# `kubectl get` would read as "OK, verified secure" too. Capturing the output
# and checking kubectl's own exit code first closes that gap.
if ! SANDBOX_POD_YAML="$(kubectl -n "${NAMESPACE}" get pod "${SANDBOX_POD}" -o yaml)"; then
  echo "ERROR: kubectl get pod ${SANDBOX_POD} failed, so whether a gateway" >&2
  echo "       token is present in the pod spec could not be verified — this" >&2
  echo "       is an inconclusive check, not a confirmed pass." >&2
  exit 1
fi
if grep -qi 'EST_GATEWAY_TOKEN' <<<"${SANDBOX_POD_YAML}"; then
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
