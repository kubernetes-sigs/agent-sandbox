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

# Smoke test for the agents-as-a-service example: runs the full README
# walkthrough non-interactively against the current kubectl context.
# Expects agent-sandbox (with extensions) already installed.
set -euo pipefail
cd "$(dirname "$0")"

NS=hermes-demo
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1" >&2; exit 1; }
cleanup() { kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "=== 1. install prereqs (throwaway credentials) + template + warm pool"
kubectl apply -f 00-prereqs.yaml
# Platform credentials are never committed; generate throwaway values.
kubectl -n "$NS" delete secret hermes-platform-secrets --ignore-not-found >/dev/null
kubectl -n "$NS" create secret generic hermes-platform-secrets \
  --from-literal=dashboard-username=platform \
  --from-literal=dashboard-password="$(openssl rand -hex 16)" \
  --from-literal=dashboard-session-secret="$(openssl rand -hex 32)" \
  --from-literal=api-server-key="$(openssl rand -hex 24)"
kubectl apply -f 10-sandboxtemplate.yaml -f 20-sandboxwarmpool.yaml
for _ in $(seq 1 120); do
  READY=$(kubectl -n "$NS" get sandboxes -l agents.x-k8s.io/warm-pool-sandbox \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
  [ "$READY" -ge 2 ] && break
  sleep 5
done
[ "${READY:-0}" -ge 2 ] || fail "warm pool did not reach 2 Ready spares"
pass "warm pool: 2 spares Ready"

echo "=== 2. signup via claim (warm adoption)"
T0=$(date +%s)
kubectl apply -f 30-claim-alice.yaml
kubectl -n "$NS" wait sandboxclaim hermes-alice --for=condition=Ready --timeout=60s
ELAPSED=$(( $(date +%s) - T0 ))
SB=$(kubectl -n "$NS" get sandboxclaim hermes-alice -o jsonpath='{.status.sandbox.name}')
case "$SB" in hermes-pool-*) ;; *) fail "expected warm adoption, got sandbox '$SB'";; esac
pass "claim Ready in ${ELAPSED}s, adopted $SB"

echo "=== 3. write state on the PVC"
POD=$(kubectl -n "$NS" get sandbox "$SB" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl -n "$NS" exec "$POD" -- sh -c 'echo "remember me" > /opt/data/marker.txt'
pass "marker written in $POD"

echo "=== 4. suspend (pod deleted, PVC + Service retained)"
kubectl -n "$NS" patch sandbox "$SB" --type merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n "$NS" wait --for=delete "pod/$POD" --timeout=120s
kubectl -n "$NS" get pvc "data-$SB" >/dev/null || fail "PVC was not retained"
kubectl -n "$NS" get svc "$SB" >/dev/null || fail "Service was not retained"
pass "suspended: pod gone, PVC and Service retained"

echo "=== 5. resume and verify state survived"
kubectl -n "$NS" patch sandbox "$SB" --type merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n "$NS" wait sandbox "$SB" --for=condition=Ready --timeout=180s
POD=$(kubectl -n "$NS" get sandbox "$SB" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
MARKER=$(kubectl -n "$NS" exec "$POD" -- cat /opt/data/marker.txt)
[ "$MARKER" = "remember me" ] || fail "marker did not survive suspend/resume"
pass "resumed: state survived on the reattached PVC"

echo "=== 6. injection policy rejects env-carrying claims"
kubectl apply -f 40-claim-rejected.yaml
# Wait for the POSITIVE rejection signal, not merely "not bound yet" — a
# broken policy that binds after a few seconds must fail this test.
REASON=""
for _ in $(seq 1 12); do
  REASON=$(kubectl -n "$NS" get sandboxclaim hermes-mallory \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)
  [ "$REASON" = "EnvVarsInjectionRejected" ] && break
  sleep 5
done
[ "$REASON" = "EnvVarsInjectionRejected" ] \
  || fail "claim was not rejected (Ready reason='$REASON')"
BOUND=$(kubectl -n "$NS" get sandboxclaim hermes-mallory -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)
[ -z "$BOUND" ] || fail "env-carrying claim unexpectedly bound sandbox '$BOUND'"
kubectl delete -f 40-claim-rejected.yaml >/dev/null
pass "claim rejected with EnvVarsInjectionRejected and never bound"

echo "=== 7. gateway: signup, proxy, idle-suspend, wake-on-connect"
if command -v docker >/dev/null && command -v kind >/dev/null; then
  KIND_CLUSTER="${KIND_CLUSTER:-$(kubectl config current-context | sed 's/^kind-//')}"
  docker build -q -t aaas-gateway:demo gateway/ >/dev/null
  kind load docker-image aaas-gateway:demo --name "$KIND_CLUSTER" >/dev/null
  kubectl apply -f 50-gateway.yaml
  kubectl -n "$NS" rollout status deploy/aaas-gateway --timeout=120s
  kubectl -n "$NS" port-forward svc/aaas-gateway 18080:8080 >/dev/null 2>&1 &
  PF=$!
  sleep 3
  GW=localhost:18080

  SIGNUP=$(curl -s -X POST "http://$GW/users" -H 'Content-Type: application/json' -d '{"user":"bob"}')
  TOKEN=$(echo "$SIGNUP" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "$TOKEN" ] || fail "signup returned no token: $SIGNUP"
  echo "$SIGNUP" | grep -q '"state":"Ready"' || fail "signup not Ready: $SIGNUP"
  pass "gateway signup: bob Ready with one-time token"

  curl -sf -H "Authorization: Bearer $TOKEN" "http://$GW/u/bob/v1/models" >/dev/null \
    || fail "proxied API call failed"
  curl -s -o /dev/null -w '%{http_code}' "http://$GW/u/bob/v1/models" | grep -q 401 \
    || fail "tokenless request was not rejected"
  pass "gateway proxy: authed request 200, tokenless 401"

  echo "waiting for idle sweeper (~75s)..."
  for _ in $(seq 1 30); do
    STATE=$(curl -s -H "Authorization: Bearer $TOKEN" "http://$GW/users/bob" \
      | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')
    [ "$STATE" = "Suspended" ] && break
    sleep 5
  done
  [ "$STATE" = "Suspended" ] || fail "bob was not idle-suspended (state=$STATE)"
  pass "idle sweeper suspended bob"

  T0=$(date +%s)
  curl -sf -m 150 -H "Authorization: Bearer $TOKEN" "http://$GW/u/bob/v1/models" >/dev/null \
    || fail "wake-on-connect request failed"
  pass "wake-on-connect: request held and served after $(( $(date +%s) - T0 ))s"

  curl -sf -X DELETE -H "Authorization: Bearer $TOKEN" "http://$GW/users/bob" >/dev/null \
    || fail "gateway user deletion failed"
  kill "$PF" 2>/dev/null || true
elif [ "${SKIP_GATEWAY:-}" = "1" ]; then
  echo "WARNING: gateway checks SKIPPED (SKIP_GATEWAY=1) — signup, auth," >&2
  echo "WARNING: proxy, idle-suspend and wake-on-connect were NOT validated" >&2
  GATEWAY_SKIPPED=1
else
  fail "docker + kind are required for the gateway checks (set SKIP_GATEWAY=1 to run only the resource-layer checks)"
fi

echo "=== 8. cascade delete"
kubectl -n "$NS" delete sandboxclaim hermes-alice
for _ in $(seq 1 24); do
  kubectl -n "$NS" get sandbox "$SB" >/dev/null 2>&1 || break
  sleep 5
done
kubectl -n "$NS" get sandbox "$SB" >/dev/null 2>&1 && fail "sandbox not garbage-collected"
for _ in $(seq 1 24); do
  kubectl -n "$NS" get pvc "data-$SB" >/dev/null 2>&1 || break
  sleep 5
done
kubectl -n "$NS" get pvc "data-$SB" >/dev/null 2>&1 && fail "PVC not garbage-collected"
pass "claim delete cascaded: sandbox + PVC gone"

echo
if [ -n "${GATEWAY_SKIPPED:-}" ]; then
  echo "Resource-layer checks passed; gateway checks were SKIPPED."
else
  echo "All checks passed."
fi
