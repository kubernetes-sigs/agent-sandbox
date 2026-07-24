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


# ***********************************************************
# Suspension gate for CSN load tests.
#
# Blocks until at least MIN_SUSPENDED nodes report the Kubernetes node
# condition Suspended=True. Invoked by run_churn_test.sh before load starts
# (wrapped in `timeout` there, which enforces the overall deadline — the loop
# here runs until killed or satisfied). Can also be run standalone.
#
# WHY THIS GATE EXISTS: standby buffer nodes run as ACTIVE capacity for
# standby-capacity-init-time (default 5m) after creation before suspending.
# Starting load before suspension means you are testing active buffers, not
# cold standby — the comparison against the static baseline becomes invalid.
#
# Usage:
#   suspension-gate.sh <min_suspended_nodes> [poll_interval_seconds]
#   suspension-gate.sh report        # one-shot node-state snapshot, exits 0
# ***********************************************************

set -o pipefail

snapshot() {
  local states total suspended resumed
  states=$(kubectl get nodes -o custom-columns='S:.status.conditions[?(@.type=="Suspended")].status' --no-headers 2>/dev/null)
  total=$(echo "$states" | grep -c .)
  suspended=$(echo "$states" | grep -c True || true)
  resumed=$(echo "$states" | grep -c False || true)
  echo "$(date '+%Y-%m-%dT%H:%M:%S') nodes: total=${total} suspended=${suspended} resumed(prev-suspended)=${resumed} never-suspended=$((total - suspended - resumed))"
}

if [ "$1" = "report" ]; then
  snapshot
  exit 0
fi

MIN_SUSPENDED="${1:?usage: suspension-gate.sh <min_suspended_nodes> [poll_interval_seconds]}"
POLL_INTERVAL="${2:-30}"

echo "Suspension gate: waiting for >= ${MIN_SUSPENDED} nodes with condition Suspended=True (poll every ${POLL_INTERVAL}s)"
while true; do
  COUNT=$(kubectl get nodes -o custom-columns='S:.status.conditions[?(@.type=="Suspended")].status' --no-headers 2>/dev/null | grep -c True || true)
  snapshot
  if [ "${COUNT:-0}" -ge "$MIN_SUSPENDED" ]; then
    echo "Suspension gate PASSED: ${COUNT} suspended nodes >= ${MIN_SUSPENDED} required. Safe to start load."
    exit 0
  fi
  sleep "$POLL_INTERVAL"
done
