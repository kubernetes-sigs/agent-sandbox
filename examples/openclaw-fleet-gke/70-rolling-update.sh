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

# Rolling update across the fleet (test-checklist item 4).
#
# Claimed sandboxes are never updated in place: the controller does not
# mutate a running pod's image or resources. Instead, an update is a batched
# RE-CLAIM: after the SandboxTemplate changes (image bump, CPU/memory
# change), the warm pool (updateStrategy: Recreate) refreshes its spares
# immediately, and this script swaps each employee onto a fresh spare via
# the portal's /rebuild endpoint. The employee's workspace (Filestore),
# stable URL (alias) and token all persist; per-employee downtime is the
# rebuild's downtime_ms (typically a few seconds, dominated by OpenClaw's
# own restart — the claim swap itself is sub-second).
#
# At 9,000 sandboxes: RATE is the knob that turns this into an operational
# rollout window. E.g. RATE=2/s => ~75 minutes fleet-wide, with each
# individual employee down only for their own rebuild. Batches should stay
# below the warm pool's replenishment rate so claims never fall back cold.
#
# Usage:
#   PORTAL_URL=http://<gateway-ip> ADMIN_TOKEN=<token> \
#     ./70-rolling-update.sh emp-0001 emp-0002 ...
#   (or with no args: all employees, discovered from claims)
set -euo pipefail

PORTAL_URL="${PORTAL_URL:-http://localhost:8080}"
ADMIN_TOKEN="${ADMIN_TOKEN:?set ADMIN_TOKEN (fleet-admin bearer token)}"
NAMESPACE="${NAMESPACE:-openclaw-fleet}"
RATE="${RATE:-0.5}" # rebuilds per second (0.5 = one every 2s)

employees=("$@")
if [ ${#employees[@]} -eq 0 ]; then
  # Discover the fleet from its claims (oc-<employee-id>).
  mapfile -t employees < <(kubectl get sandboxclaims -n "${NAMESPACE}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sed 's/^oc-//')
fi

echo "Rolling ${#employees[@]} employee(s) at ${RATE}/s via ${PORTAL_URL}"
interval=$(awk "BEGIN {print 1 / ${RATE}}")
failures=0

for emp in "${employees[@]}"; do
  start=$(date +%s%3N)
  out=$(curl -sS -X POST "${PORTAL_URL}/employees/${emp}/rebuild" \
    -H "Authorization: Bearer ${ADMIN_TOKEN}") || { echo "FAIL ${emp}"; failures=$((failures+1)); continue; }
  end=$(date +%s%3N)
  downtime=$(echo "${out}" | sed -n 's/.*"downtime_ms": *\([0-9]*\).*/\1/p')
  echo "${emp}: downtime=${downtime:-?}ms wall=$((end - start))ms"
  sleep "${interval}"
done

echo "Done. ${failures} failure(s)."
exit "$((failures > 0 ? 1 : 0))"
