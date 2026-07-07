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

# E2E verification of the FULL path: external client -> GKE Gateway -> sandbox-router
# -> sandbox /execute -> git clone (governed by the per-identity Cilium egress policy).
#   Phase 1: team-a clone SUCCEEDS, team-b clone FAILS (denied).
#   Phase 2: apply team-b allowlist, team-b clone SUCCEEDS.
# Run after scripts/optional-gateway-routing.sh. Exits non-zero on any failed assertion.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"
NS="$NAMESPACE"

GW="$(kubectl -n "$NS" get gateway external-http-gateway -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
[ -n "$GW" ] || { echo "ERROR: gateway has no external address yet."; exit 1; }
echo "Gateway address: $GW"

# Auth: optional-gateway-routing.sh mints a Bearer token Secret by default. Read
# it (empty in the opt-in unauthenticated mode) and send it on /execute calls.
TOKEN="$(kubectl -n "$NS" get secret sandbox-router-auth -o jsonpath='{.data.auth-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
AUTH=(); [ -n "$TOKEN" ] && AUTH=(-H "Authorization: Bearer $TOKEN")

echo "Waiting for the gateway backend to pass health checks..."
code=""
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$GW/healthz" || true)"
  [ "$code" = "200" ] && break
  sleep 10
done
echo "GET http://$GW/healthz -> $code"
[ "$code" = "200" ] || { echo "ERROR: gateway not healthy."; exit 1; }

podip() { kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}'; }

# Drive a clone through the gateway+router into the sandbox; print only exit_code.
gw_clone() {
  local sb="$1" ip; ip="$(podip "$sb")"
  curl -s --max-time 90 -X POST "http://$GW/execute" \
    ${AUTH[@]+"${AUTH[@]}"} \
    -H "X-Sandbox-ID: $sb" -H "X-Sandbox-Namespace: $NS" -H "X-Sandbox-Pod-IP: $ip" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":\"timeout 25 git clone --depth 1 $TEST_REPO_URL /tmp/r-$$-$RANDOM\"}" \
    | python3 -c 'import sys,json
try:
    print(json.load(sys.stdin).get("exit_code","ERR"))
except Exception:
    print("ERR")'
}

# Poll gw_clone until it reaches the expected state (policy changes are async),
# or give up after ~90s. Returns the last observed exit code.
poll_clone() { # sandbox op(eq0|ne0)
  local sb="$1" op="$2" a=""
  for _ in $(seq 1 18); do
    a="$(gw_clone "$sb")"
    if { [ "$op" = eq0 ] && [ "$a" = 0 ]; } || \
       { [ "$op" = ne0 ] && [ "$a" != 0 ] && [ "$a" != ERR ]; }; then
      break
    fi
    sleep 5
  done
  echo "$a"
}

pass=0; fail=0
check() { # desc actual op(eq0|ne0)
  local d="$1" a="$2" op="$3"
  # Empty/non-numeric means the gateway call itself failed — never a real "deny".
  case "$a" in
    ''|*[!0-9]*) echo "FAIL: $d (no numeric exit code: '$a')"; fail=$((fail+1)); return;;
  esac
  if { [ "$op" = eq0 ] && [ "$a" = 0 ]; } || { [ "$op" = ne0 ] && [ "$a" != 0 ]; }; then
    echo "PASS: $d (exit=$a)"; pass=$((pass+1))
  else echo "FAIL: $d (exit=$a)"; fail=$((fail+1)); fi
}

# Reset to phase 1 so this is re-runnable.
kubectl delete -f "$M/40-cilium-team-b-allow-github.yaml" --ignore-not-found >/dev/null 2>&1 || true

echo "== Phase 1 (through the gateway) =="
check "team-a ALLOWED to clone github via gateway"  "$(poll_clone sandbox-team-a eq0)" eq0
check "team-b DENIED from cloning github via gateway" "$(poll_clone sandbox-team-b ne0)" ne0

echo
echo "== Phase 2: allowlist team-b =="
kubectl apply -f "$M/40-cilium-team-b-allow-github.yaml"
check "team-b ALLOWED after allowlist via gateway" "$(poll_clone sandbox-team-b eq0)" eq0

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
echo "Full ingress + egress flow verified. 🎉"
