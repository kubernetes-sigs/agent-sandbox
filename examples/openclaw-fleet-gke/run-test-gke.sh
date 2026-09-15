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

# End-to-end verification on a GKE cluster provisioned by
# setup/provision-gke.sh. Asserts one section per P0 item of the fleet
# test checklist:
#   [1] warm-pool startup + sub-second claim (timing breakdown printed)
#   [2] deletion releases all resources (claim, sandbox, pod, alias, data)
#   [3] sleep releases the pod; wake-up duration recorded; state persists
#   [4] template update -> pool refresh -> per-employee rebuild w/ downtime
#   [5] late-bound per-employee Filestore workspace (persistence proven)
#   [6] stable per-employee URL through Gateway + sandbox-router
# Pod-snapshot (memory) hibernation is a manual runbook: 60-snapshots/.
#
# Requires: kubectl context on the PoC cluster; docker + push access to
# IMAGE_REPO (Artifact Registry) for the portal image.
#
# Usage:
#   IMAGE_REPO=us-central1-docker.pkg.dev/<project>/fleet ./run-test-gke.sh
set -euo pipefail
cd "$(dirname "$0")"

NS=openclaw-fleet
IMAGE_REPO="${IMAGE_REPO:?set IMAGE_REPO (registry path for the portal image)}"
PORTAL_IMAGE="${IMAGE_REPO}/fleet-portal:demo"
SKIP_GATEWAY="${SKIP_GATEWAY:-false}" # true => skip [6] (LB provisioning ~10 min)
EMP="emp-e2e-$(date +%s | tail -c 5)"

log()  { echo; echo "### $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

log "0. Prereqs: secrets, storage, template, pool"
kubectl apply -f 00-prereqs.yaml
kubectl -n "${NS}" get secret storage-daemon-token >/dev/null 2>&1 || \
  kubectl -n "${NS}" create secret generic storage-daemon-token \
    --from-literal=token="$(openssl rand -hex 24)"
# Always resync: on re-runs a stale secret would 401 this run's token. The
# portal reads it via env, so it is restarted after deploy below.
ADMIN_TOKEN="${ADMIN_TOKEN:-$(openssl rand -hex 24)}"
kubectl -n "${NS}" create secret generic fleet-admin \
  --from-literal=sha256="$(printf %s "${ADMIN_TOKEN}" | sha256sum | cut -d' ' -f1)" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f 10-storage.yaml
# First Filestore provision takes several minutes; the daemon can't start
# until the RWX volume binds.
kubectl -n "${NS}" wait --for=jsonpath='{.status.phase}'=Bound \
  pvc/fleet-master-pvc --timeout=15m
kubectl -n "${NS}" rollout status ds/storage-node-daemon --timeout=5m
kubectl apply -f 20-openclaw-template.yaml -f 30-warmpool.yaml
# Warm spares are Ready while spin-waiting for their late-bind signal.
for i in $(seq 1 60); do
  ready=$(kubectl -n "${NS}" get sandboxwarmpool openclaw-fleet-pool \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
  [ "${ready:-0}" -ge 5 ] && break
  sleep 5
done
[ "${ready:-0}" -ge 5 ] || fail "warm pool never reached 5 ready spares"
echo "warm pool ready: ${ready}/5"

log "0b. Fleet portal"
if [ "${SKIP_IMAGE_BUILD:-false}" != "true" ]; then
  # GKE nodes are amd64; force the platform so arm64 workstations work too.
  # (No local docker? Push once with:
  #   gcloud builds submit --tag ${PORTAL_IMAGE} 40-fleet-portal/
  # and re-run with SKIP_IMAGE_BUILD=true.)
  docker build -q --platform "${DOCKER_PLATFORM:-linux/amd64}" \
    -t "${PORTAL_IMAGE}" 40-fleet-portal/
  docker push -q "${PORTAL_IMAGE}"
fi
sed "s|image: fleet-portal:demo|image: ${PORTAL_IMAGE}|" \
  40-fleet-portal/portal.yaml | kubectl apply -f -
# Restart so the pod resolves the (possibly resynced) fleet-admin secret.
kubectl -n "${NS}" rollout restart deploy/fleet-portal
kubectl -n "${NS}" rollout status deploy/fleet-portal --timeout=5m
kubectl apply -f 50-router/10-router.yaml
kubectl -n "${NS}" rollout status deploy/sandbox-router --timeout=5m
if [ "${SKIP_GATEWAY}" != "true" ]; then
  kubectl apply -f 50-router/20-gateway.yaml
fi
# Drive the portal API through a port-forward: [6] tests the Gateway path
# separately, and this keeps [1]-[5] independent of LB provisioning time.
kubectl -n "${NS}" port-forward svc/fleet-portal 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill ${PF_PID} 2>/dev/null || true' EXIT
sleep 3
PORTAL=http://localhost:18080

log "1. Warm-pool startup: provision ${EMP} (sub-second claim)"
out=$(curl -fsS -X POST "${PORTAL}/employees" -H 'Content-Type: application/json' \
  -d "{\"employee\": \"${EMP}\"}")
echo "${out}"
TOKEN=$(echo "${out}" | sed -n 's/.*"token": *"\([^"]*\)".*/\1/p')
SANDBOX=$(echo "${out}" | sed -n 's/.*"sandbox": *"\([^"]*\)".*/\1/p')
adopted_ms=$(echo "${out}" | sed -n 's/.*"adopted_ms": *\([0-9]*\).*/\1/p')
[ -n "${SANDBOX}" ] || fail "no sandbox adopted"
# The claim swap itself is the sub-second promise. 2000ms is the assertion
# ceiling to keep CI honest on a busy cluster; record the actual number.
[ "${adopted_ms}" -lt 2000 ] || fail "adoption took ${adopted_ms}ms (expected sub-second)"
# Warm (not cold) provenance:
lt=$(kubectl -n "${NS}" get sandbox "${SANDBOX}" \
  -o jsonpath='{.metadata.labels.agents\.x-k8s\.io/launch-type}')
[ "${lt}" = "warm" ] || fail "sandbox was not a warm adoption (launch-type=${lt})"
echo "PASS [1]: warm adoption in ${adopted_ms}ms"

log "5. Late-bound workspace: write state, prove persistence below"
POD=$(kubectl -n "${NS}" get pod -l sandbox.users.io/employee="${EMP}" \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n "${NS}" exec "${POD}" -c openclaw -- \
  sh -c 'echo "poc-marker-$(hostname)" > /workspace/.openclaw/poc-marker.txt'
kubectl -n "${NS}" exec "${POD}" -c openclaw -- test -f /workspace/.openclaw/poc-marker.txt
echo "PASS [5]: workspace bound and writable at /workspace/.openclaw"

log "3. Sleep & wake: suspend releases the pod, wake restores state"
curl -fsS -X POST "${PORTAL}/employees/${EMP}/suspend" \
  -H "Authorization: Bearer ${TOKEN}" >/dev/null
kubectl -n "${NS}" wait sandbox/"${SANDBOX}" \
  --for=condition=Suspended --timeout=3m
n_pods=$(kubectl -n "${NS}" get pod -l sandbox.users.io/employee="${EMP}" \
  --no-headers 2>/dev/null | grep -cv Terminating || true)
[ "${n_pods}" = "0" ] || fail "pod still present while Suspended"
echo "suspended: pod released, Service + alias + workspace retained"
out=$(curl -fsS -X POST "${PORTAL}/employees/${EMP}/wake" \
  -H "Authorization: Bearer ${TOKEN}")
wake_ms=$(echo "${out}" | sed -n 's/.*"wake_ms": *\([0-9]*\).*/\1/p')
POD=$(kubectl -n "${NS}" get pod -l sandbox.users.io/employee="${EMP}" \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
kubectl -n "${NS}" exec "${POD}" -c openclaw -- \
  test -f /workspace/.openclaw/poc-marker.txt || fail "state lost across sleep/wake"
echo "PASS [3]: wake in ${wake_ms}ms, state intact"

log "4. Update: bump template, pool recreates spares, rebuild employee"
# A revision env var changes the blueprint hash exactly like an image bump
# (same mechanism, no need to publish a second OpenClaw image for the test).
kubectl -n "${NS}" patch sandboxtemplate openclaw-fleet-template --type=json -p '[
  {"op": "add",
   "path": "/spec/podTemplate/spec/containers/0/env/-",
   "value": {"name": "FLEET_TEMPLATE_REV", "value": "2"}}]'
sleep 5 # Recreate strategy: stale spares deleted immediately
for i in $(seq 1 60); do
  ready=$(kubectl -n "${NS}" get sandboxwarmpool openclaw-fleet-pool \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
  [ "${ready:-0}" -ge 5 ] && break
  sleep 5
done
[ "${ready:-0}" -ge 5 ] || fail "pool did not recover after template update"
out=$(curl -fsS -X POST "${PORTAL}/employees/${EMP}/rebuild" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}")
downtime_ms=$(echo "${out}" | sed -n 's/.*"downtime_ms": *\([0-9]*\).*/\1/p')
POD=$(kubectl -n "${NS}" get pod -l sandbox.users.io/employee="${EMP}" \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
kubectl -n "${NS}" exec "${POD}" -c openclaw -- sh -c \
  '[ "$FLEET_TEMPLATE_REV" = "2" ]' || fail "rebuilt pod is not on the new template"
kubectl -n "${NS}" exec "${POD}" -c openclaw -- \
  test -f /workspace/.openclaw/poc-marker.txt || fail "state lost across rebuild"
echo "PASS [4]: rolled to new template with ${downtime_ms}ms downtime, state intact"

if [ "${SKIP_GATEWAY}" != "true" ]; then
  log "6. Stable per-employee URL via Gateway + sandbox-router"
  GW_IP=""
  for i in $(seq 1 90); do
    GW_IP=$(kubectl -n "${NS}" get gateway openclaw-fleet-gateway \
      -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)
    [ -n "${GW_IP}" ] && break
    sleep 10
  done
  [ -n "${GW_IP}" ] || fail "Gateway never got an address"
  # Any HTTP status proves routing reached OpenClaw; LB programming of a
  # fresh route can lag the address by a couple of minutes.
  for i in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' \
      "http://${GW_IP}/router/${NS}/oc-${EMP}/18789/" || true)
    [ "${code}" != "000" ] && [ "${code}" != "404" ] && [ "${code}" != "502" ] && break
    sleep 10
  done
  [ "${code}" != "000" ] && [ "${code}" != "404" ] && [ "${code}" != "502" ] \
    || fail "gateway route not serving (last code ${code})"
  echo "PASS [6]: http://${GW_IP}/router/${NS}/oc-${EMP}/18789/ -> HTTP ${code}"
fi

log "2. Deletion: everything released, workspace purged"
curl -fsS -X DELETE "${PORTAL}/employees/${EMP}?purge=true" \
  -H "Authorization: Bearer ${TOKEN}" >/dev/null
for i in $(seq 1 30); do
  kubectl -n "${NS}" get sandboxclaim "oc-${EMP}" >/dev/null 2>&1 || break
  sleep 2
done
kubectl -n "${NS}" get sandboxclaim "oc-${EMP}" >/dev/null 2>&1 && fail "claim still present"
kubectl -n "${NS}" get sandbox "${SANDBOX}" >/dev/null 2>&1 && fail "sandbox still present"
kubectl -n "${NS}" get svc "oc-${EMP}" >/dev/null 2>&1 && fail "alias still present"
DAEMON=$(kubectl -n "${NS}" get pod -l app=storage-node-daemon \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n "${NS}" exec "${DAEMON}" -- \
  test ! -d "/mnt/master-volume/users/${EMP}" || fail "workspace not purged"
echo "PASS [2]: claim, sandbox, pod, alias and workspace all released"

echo
echo "ALL CHECKS PASSED"
echo "  [1] warm adoption:      ${adopted_ms}ms"
echo "  [3] wake:               ${wake_ms}ms"
echo "  [4] rebuild downtime:   ${downtime_ms}ms"
