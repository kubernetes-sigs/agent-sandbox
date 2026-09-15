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

# Startup-latency measurement (test-checklist item 1: expected vs actual).
#
# Provisions N employees through the portal and summarizes the portal's
# per-phase timing breakdown:
#   adopted_ms   claim created -> warm sandbox adopted (the SUB-SECOND part:
#                pure API metadata writes against an already-running pod)
#   bound_ms     adoption -> Filestore workspace bind-mounted + .ready
#   app_ready_ms bind -> OpenClaw gateway answering HTTP
#   total_ms     end-to-end, what an employee actually waits at signup
#
# Keep N below the warm pool's replicas or the tail falls back to cold
# starts — which is itself worth measuring once, for the comparison row.
# For controller-side ground truth (creationTimestamp is second-truncated),
# deploy ../../webhook-inject-timestamp and compare against these numbers.
#
# Usage: PORTAL_URL=http://<gateway-ip> ./measure-claim-latency.sh 5
set -euo pipefail

PORTAL_URL="${PORTAL_URL:-http://localhost:8080}"
N="${1:-5}"
PREFIX="${PREFIX:-bench-$(date +%s)}"
results="$(mktemp)"

for i in $(seq 1 "${N}"); do
  emp="${PREFIX}-$(printf %04d "${i}")"
  out=$(curl -sS -X POST "${PORTAL_URL}/employees" \
    -H 'Content-Type: application/json' \
    -d "{\"employee\": \"${emp}\"}")
  echo "${out}" >> "${results}"
  echo "${emp}: $(echo "${out}" | sed -n 's/.*"timings": *{\([^}]*\)}.*/\1/p')"
done

echo
echo "=== summary over ${N} provisions (ms) ==="
for phase in adopted_ms bound_ms app_ready_ms total_ms; do
  # min / p50 / p90 / max per phase
  sed -n "s/.*\"${phase}\": *\([0-9]*\).*/\1/p" "${results}" | sort -n | awk -v phase="${phase}" '
    {v[NR]=$1}
    END {
      if (NR == 0) { print phase": no data"; exit }
      printf "%-14s min=%-6d p50=%-6d p90=%-6d max=%-6d\n", phase, v[1], v[int((NR+1)/2)], v[int(NR*0.9) < 1 ? 1 : int(NR*0.9)], v[NR]
    }'
done

echo
echo "Cleanup: for e in \$(kubectl get sandboxclaims -n openclaw-fleet -o name | grep oc-${PREFIX}); do ...; done"
echo "  (or DELETE /employees/<id>?purge=true per employee via the portal)"
rm -f "${results}"
