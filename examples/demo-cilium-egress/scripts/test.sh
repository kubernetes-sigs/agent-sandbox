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

# E2E verification of per-identity egress.
#   Phase 1: team-a clone SUCCEEDS, team-b clone FAILS (denied).
#   Phase 2: apply team-b allowlist, team-b clone SUCCEEDS.
# Exits non-zero if any assertion fails.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"
NS="$NAMESPACE"

pass=0; fail=0

# Run a clone inside a sandbox; print only the exit code (0 = success).
clone() {
  kubectl exec -n "$NS" "$1" -- sh -c \
    "rm -rf /tmp/r; timeout 40 git clone --depth 1 $TEST_REPO_URL /tmp/r >/dev/null 2>&1; echo \$?" \
    2>/dev/null | tail -n1
}

check() { # desc, actual, expected_op (eq0|ne0)
  local desc="$1" actual="$2" op="$3"
  # An empty/non-numeric result means the exec itself failed (pod not ready,
  # API error) — never let that masquerade as a passing "denied" assertion.
  case "$actual" in
    ''|*[!0-9]*) echo "FAIL: $desc (no numeric exit code: '$actual')"; fail=$((fail+1)); return;;
  esac
  if { [ "$op" = "eq0" ] && [ "$actual" = "0" ]; } || { [ "$op" = "ne0" ] && [ "$actual" != "0" ]; }; then
    echo "PASS: $desc (exit=$actual)"; pass=$((pass+1))
  else
    echo "FAIL: $desc (exit=$actual)"; fail=$((fail+1))
  fi
}

# Reset to phase-1 state so this script is re-runnable (remove any prior team-b allow).
kubectl delete -f "$M/40-cilium-team-b-allow-github.yaml" --ignore-not-found >/dev/null 2>&1 || true
sleep 4

echo "== Phase 1 =="
check "team-a is ALLOWED to clone github"  "$(clone sandbox-team-a)" eq0
check "team-b is DENIED from cloning github" "$(clone sandbox-team-b)" ne0

echo
echo "== Phase 2: allowlist team-b's identity =="
kubectl apply -f "$M/40-cilium-team-b-allow-github.yaml"
echo "Waiting for the new policy to take effect..."
sleep 8
check "team-b is now ALLOWED after allowlist" "$(clone sandbox-team-b)" eq0

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || { echo "Tip: 'cilium hubble enable' then 'hubble observe -n $NS --verdict DROPPED' to see drops."; exit 1; }
echo "All assertions passed. 🎉"
