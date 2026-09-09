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
# Builds the demo image, loads it into a kind cluster, applies the Sandbox, and
# asserts that nono enforced the expected boundaries. Assumes the agent-sandbox
# controller/CRDs are already installed in the cluster.

set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUDIT_ARTIFACT_ROOT="${AUDIT_ARTIFACT_ROOT:-${SCRIPT_DIR}/audit-artifacts}"
KEEP_AUDIT_PVC="${KEEP_AUDIT_PVC:-true}"
RUN_STARTED_EPOCH="$(date -u +%s)"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$"
AUDIT_ARTIFACT_DIR="${AUDIT_ARTIFACT_ROOT}/${RUN_ID}"
AUDIT_KEY_DIR=""
cd "${SCRIPT_DIR}"

cleanup_local_key() {
  if [ -n "${AUDIT_KEY_DIR}" ] && [ -d "${AUDIT_KEY_DIR}" ]; then
    rm -rf -- "${AUDIT_KEY_DIR}"
  fi
}
trap cleanup_local_key EXIT

# Derive the image tag from the manifest so the two never drift.
IMAGE="$(grep -E '^[[:space:]]+image:' sandbox.yaml | head -1 | awk '{print $2}' || true)"
if [ -z "${IMAGE}" ]; then
  echo "ERROR: could not read image tag from sandbox.yaml" >&2
  exit 1
fi
# --- Build & load -----------------------------------------------------------

echo "Building ${IMAGE}..."
docker build -t "${IMAGE}" .

echo "Loading ${IMAGE} into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image "${IMAGE}" --name "${KIND_CLUSTER_NAME}"

# --- Ephemeral audit identity -----------------------------------------------

echo "Generating an ephemeral P-256 audit-signing identity..."
AUDIT_KEY_DIR="$(mktemp -d "${TMPDIR:-/tmp}/nono-audit-key.XXXXXX")"
PRIVATE_KEY_PEM="${AUDIT_KEY_DIR}/private.pem"
PRIVATE_KEY_B64="${AUDIT_KEY_DIR}/private.pk8.b64"
PUBLIC_KEY_PEM="${AUDIT_KEY_DIR}/audit-signing-public.pem"
LOKI_TLS_CA_KEY="${AUDIT_KEY_DIR}/loki-ca.key"
LOKI_TLS_CA_CERT="${AUDIT_KEY_DIR}/loki-ca.crt"
LOKI_TLS_KEY="${AUDIT_KEY_DIR}/loki-tls.key"
LOKI_TLS_CSR="${AUDIT_KEY_DIR}/loki-tls.csr"
LOKI_TLS_CERT="${AUDIT_KEY_DIR}/loki-tls.crt"
LOKI_TLS_EXT="${AUDIT_KEY_DIR}/loki-tls.ext"

openssl genpkey \
  -algorithm EC \
  -pkeyopt ec_paramgen_curve:P-256 \
  -out "${PRIVATE_KEY_PEM}"
openssl pkcs8 -topk8 -nocrypt -in "${PRIVATE_KEY_PEM}" -outform DER \
  | openssl base64 -A >"${PRIVATE_KEY_B64}"
openssl pkey -in "${PRIVATE_KEY_PEM}" -pubout -out "${PUBLIC_KEY_PEM}"

echo "Generating an ephemeral CA and TLS identity for the demo Loki Service..."
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
  -subj "/CN=nono-demo-loki-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -keyout "${LOKI_TLS_CA_KEY}" \
  -out "${LOKI_TLS_CA_CERT}"
openssl req -new -newkey rsa:2048 -sha256 -nodes \
  -subj "/CN=nono-demo-loki" \
  -keyout "${LOKI_TLS_KEY}" \
  -out "${LOKI_TLS_CSR}"
printf '%s\n' \
  'basicConstraints=critical,CA:FALSE' \
  'keyUsage=critical,digitalSignature,keyEncipherment' \
  'extendedKeyUsage=serverAuth' \
  'subjectAltName=DNS:nono-demo-loki' \
  >"${LOKI_TLS_EXT}"
openssl x509 -req -sha256 -days 1 \
  -in "${LOKI_TLS_CSR}" \
  -CA "${LOKI_TLS_CA_CERT}" \
  -CAkey "${LOKI_TLS_CA_KEY}" \
  -CAcreateserial \
  -extfile "${LOKI_TLS_EXT}" \
  -out "${LOKI_TLS_CERT}"
openssl verify \
  -CAfile "${LOKI_TLS_CA_CERT}" \
  -verify_hostname nono-demo-loki \
  "${LOKI_TLS_CERT}"

mkdir -p "${AUDIT_ARTIFACT_DIR}"
cp "${PUBLIC_KEY_PEM}" "${AUDIT_ARTIFACT_DIR}/audit-signing-public.pem"

# --- Apply ------------------------------------------------------------------

cleanup() {
  echo "Cleaning up..."
  set +e
  kubectl delete --ignore-not-found -f audit-verifier.yaml
  kubectl delete --ignore-not-found -f sandbox.yaml
  kubectl delete --ignore-not-found -f loki.yaml
  kubectl delete --ignore-not-found configmap nono-demo-audit-public-key
  kubectl delete --ignore-not-found secret nono-demo-audit-signing-key
  kubectl delete --ignore-not-found secret nono-demo-loki-tls
  kubectl delete --ignore-not-found -f nono-profiles-configmap.yaml
  kubectl delete --ignore-not-found -f demo-llm-secret.yaml
  if [ "${KEEP_AUDIT_PVC}" != "true" ]; then
    kubectl delete --ignore-not-found -f audit-storage.yaml
  fi
  if [ -n "${AUDIT_KEY_DIR}" ] && [ -d "${AUDIT_KEY_DIR}" ]; then
    rm -rf -- "${AUDIT_KEY_DIR}"
  fi
  set -e
}
trap cleanup EXIT

echo "Applying manifests..."
# Recover cleanly from an interrupted previous demo while retaining its audit PVC.
kubectl delete --ignore-not-found -f audit-verifier.yaml
kubectl delete --ignore-not-found -f sandbox.yaml
kubectl delete --ignore-not-found -f loki.yaml
kubectl apply -f audit-storage.yaml
kubectl create secret generic nono-demo-audit-signing-key \
  --from-file="private.pk8.b64=${PRIVATE_KEY_B64}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create configmap nono-demo-audit-public-key \
  --from-file="audit-signing-public.pem=${PUBLIC_KEY_PEM}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic nono-demo-loki-tls \
  --from-file="ca.crt=${LOKI_TLS_CA_CERT}" \
  --from-file="tls.crt=${LOKI_TLS_CERT}" \
  --from-file="tls.key=${LOKI_TLS_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f demo-llm-secret.yaml
kubectl apply -f nono-profiles-configmap.yaml
kubectl apply -f loki.yaml
echo "Waiting for Loki and its seeded incident log..."
kubectl rollout status deployment/nono-demo-loki --timeout=120s
kubectl wait --for=condition=complete job/nono-demo-loki-seed --timeout=120s
kubectl apply -f sandbox.yaml

echo "Waiting for Sandbox to be Ready..."
kubectl wait --for=condition=Ready sandbox/nono-agent --timeout=120s

# --- Assert -----------------------------------------------------------------

echo "Reading workload output..."
POD="$(kubectl get pod -l sandbox=nono-agent -o jsonpath='{.items[0].metadata.name}')"
LOGS="$(kubectl logs "${POD}")"
echo "----------------------------------------"
echo "${LOGS}"
echo "----------------------------------------"

fail=0
assert() {
  if echo "${LOGS}" | grep -qF "$1"; then
    echo "PASS: $1"
  else
    echo "FAIL: expected to see: $1" >&2
    fail=1
  fi
}

# nono fails closed if the node kernel lacks Landlock; surface that clearly
# rather than reporting a confusing assertion failure.
if echo "${LOGS}" | grep -qiE "landlock.*(unavailable|unsupported|not (available|supported))"; then
  cat >&2 <<EOF
NOTE: nono reports Landlock is unavailable on this node's kernel, so it failed
closed on the filesystem controls (this is the intended safe behavior). Run on a
node whose kernel has Landlock (Linux >= 5.13 with landlock in the active LSM
list; check with: cat /sys/kernel/security/lsm) to see filesystem enforcement.
The network/credential controls do not depend on Landlock.
EOF
  exit 1
fi

assert "[ok]    wrote /workspace/hello.txt"
assert "[policy-ok] blocked read of mounted /etc/secret-config"
assert "[policy-ok] blocked read of protected audit state"
assert "[credential-ok] agent received a session-scoped phantom, not the provider key"
assert "[tool-ok] Loki identity and address are absent from the agent loop"
assert "[tool-ok] exact LogCLI incident query returned the seeded Loki log"
assert "[tool-ok] L7 policy blocked LogCLI from the labels endpoint"
assert "[tool-ok] invocation policy blocked altered LogCLI arguments"
assert "[tool-ok] invocation policy blocked LogCLI deletion management"
assert "[policy-ok] blocked egress to non-allow-listed host (example.com)"
assert "[policy-ok] blocked allow-listed host at disallowed path (/v1/models)"

if [ "${fail}" -ne 0 ]; then
  echo "One or more assertions failed." >&2
  exit 1
fi
echo "All assertions passed: nono enforced the expected boundaries inside the Sandbox."

# --- Finalize and verify signed audit ---------------------------------------

echo "Finalizing the signed audit session..."
INITIAL_RESTART_COUNT="$(kubectl get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
kubectl exec "${POD}" -- /usr/bin/touch /workspace/.finish-demo

AUDIT_FINALIZED=false
for _ in $(seq 1 60); do
  if ! kubectl get pod "${POD}" >/dev/null 2>&1; then
    AUDIT_FINALIZED=true
    break
  fi
  CURRENT_RESTART_COUNT="$(
    kubectl get pod "${POD}" \
      -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || true
  )"
  if [ -n "${CURRENT_RESTART_COUNT}" ] \
    && [ "${CURRENT_RESTART_COUNT}" -gt "${INITIAL_RESTART_COUNT}" ]; then
    AUDIT_FINALIZED=true
    break
  fi
  TERMINATED_EXIT_CODE="$(
    kubectl get pod "${POD}" \
      -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' \
      2>/dev/null || true
  )"
  if [ "${TERMINATED_EXIT_CODE}" = "0" ]; then
    AUDIT_FINALIZED=true
    break
  fi
  sleep 1
done

if [ "${AUDIT_FINALIZED}" != "true" ]; then
  echo "FAIL: workload did not exit in time for audit finalization" >&2
  exit 1
fi

# Stop the Sandbox to release the ReadWriteOnce audit PVC, then mount it
# read-only in a separate verifier pod.
kubectl delete --ignore-not-found -f sandbox.yaml
kubectl wait --for=delete "pod/${POD}" --timeout=120s
kubectl apply -f audit-verifier.yaml
kubectl wait --for=condition=Ready pod/nono-audit-verifier --timeout=120s

# Audit sessions become discoverable only after nono has finalized them. Select
# the successful workload session from this run rather than trusting ordering or
# accidentally accepting an older retained session.
AUDIT_LIST="$(
  kubectl exec nono-audit-verifier -- \
    nono audit list --recent 20 --json --silent
)"
AUDIT_SESSION_ID="$(printf '%s\n' "${AUDIT_LIST}" | python3 -c '
import datetime, json, sys
sessions = json.load(sys.stdin)
run_started = int(sys.argv[1])
for session in sessions:
    started = datetime.datetime.fromisoformat(
        session["started"].replace("Z", "+00:00")
    ).timestamp()
    command = session.get("command", [])
    if (
        started >= run_started
        and session.get("ended")
        and session.get("exit_code") == 0
        and session.get("network_event_count", 0) >= 2
        and "/opt/agent/workload.py" in command
    ):
        print(session["session_id"])
        break
else:
    raise SystemExit("no finalized audit session found for this demo run")
' "${RUN_STARTED_EPOCH}")"
echo "Verifying signed audit session ${AUDIT_SESSION_ID}..."

VERIFY_JSON="$(
  kubectl exec nono-audit-verifier -- \
    nono audit verify "${AUDIT_SESSION_ID}" \
      --public-key-file /etc/nono/audit-public-key/audit-signing-public.pem \
      --json --silent
)"
SESSION_JSON="$(
  kubectl exec nono-audit-verifier -- \
    nono audit show "${AUDIT_SESSION_ID}" --json --silent
)"

printf '%s\n' "${VERIFY_JSON}" >"${AUDIT_ARTIFACT_DIR}/verification.json"
printf '%s\n' "${SESSION_JSON}" >"${AUDIT_ARTIFACT_DIR}/session.json"
python3 verify-audit.py \
  "${AUDIT_ARTIFACT_DIR}/verification.json" \
  "${AUDIT_ARTIFACT_DIR}/session.json" \
  "${AUDIT_SESSION_ID}"

# Export the complete audit root (session event log, ledger, and signed bundle)
# so it remains verifiable even after the kind cluster is removed.
mkdir -p "${AUDIT_ARTIFACT_DIR}/state"
kubectl exec nono-audit-verifier -- \
  tar -C /var/lib/nono-state -cf - nono/audit \
  | tar -C "${AUDIT_ARTIFACT_DIR}/state" -xf -

echo "Signed audit retained in PVC 'nono-demo-audit'."
echo "Portable audit artifacts: ${AUDIT_ARTIFACT_DIR}"
echo "Verify later with:"
echo "  XDG_STATE_HOME=${AUDIT_ARTIFACT_DIR}/state nono audit verify ${AUDIT_SESSION_ID} --public-key-file ${AUDIT_ARTIFACT_DIR}/audit-signing-public.pem"
