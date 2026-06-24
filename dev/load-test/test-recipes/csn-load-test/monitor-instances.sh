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


# ***************************************************************************
# GCE instance tracker for CSN load tests.
#
# Why: how many instances were started, which machine types / zones, WHEN
# each appeared, and whether MORE were provisioned than the workload needed
# (CSN standby refill + NAP on-demand can double-provision for the same pods).
#
# Writes two artifacts you can send for analysis:
#   instances-wide.csv   one row per (poll, instance) — full reconstruction
#   instances-events.log first-seen / status-change events per instance
#
# Usage:
#   PROJECT=gke-ai-eco-dev CLUSTER=csn-hpa-sandbox-burst ./monitor-instances.sh
#   (Ctrl-C to stop; prints a summary on exit.)
# ***************************************************************************

set -o pipefail

if ! command -v gcloud >/dev/null 2>&1; then
  echo "ERROR: gcloud CLI not found. Please install the Google Cloud SDK." >&2
  exit 1
fi

PROJECT="${PROJECT:-gke-ai-eco-dev}"
CLUSTER="${CLUSTER:-csn-hpa-sandbox-burst}"
POLL_SECONDS="${POLL_SECONDS:-15}"
RUN_ID="${RUN_ID:-$(date +%Y%m%d-%H%M%S)}"
LOG_DIR="${LOG_DIR:-/tmp/load_test_metrics/${RUN_ID}}"
mkdir -p "$LOG_DIR"

WIDE="${LOG_DIR}/instances-wide.csv"
EVENTS="${LOG_DIR}/instances-events.log"
STATE_FILE="$(mktemp)"   # name -> last status, for change detection

echo "poll_ts,name,zone,machine_type,status,creation_ts" > "$WIDE"
: > "$EVENTS"

echo "=== Instance tracker: cluster=${CLUSTER} project=${PROJECT} ==="
echo "Artifacts: ${WIDE}"
echo "           ${EVENTS}"
echo "Polling every ${POLL_SECONDS}s. Ctrl-C to stop and print summary."

summary() {
  echo ""
  echo "=== INSTANCE SUMMARY (${RUN_ID}) ==="
  # Unique instances ever seen, by machine type:
  echo "Distinct instances ever seen, by machine type:"
  tail -n +2 "$WIDE" | awk -F, '{print $2","$4}' | sort -u \
    | awk -F, '{c[$2]++} END {for (m in c) printf "  %-22s %d\n", m, c[m]}'
  TOTAL_UNIQUE=$(tail -n +2 "$WIDE" | cut -d, -f2 | sort -u | grep -c .)
  echo "  TOTAL distinct instances: ${TOTAL_UNIQUE}"
  echo ""
  # Peak concurrent RUNNING (the real cost driver):
  echo "Peak concurrent RUNNING instances:"
  tail -n +2 "$WIDE" | awk -F, '$5=="RUNNING"{print $1}' | sort | uniq -c \
    | sort -rnk1 | head -1 | awk '{printf "  %d running at poll_ts=%s\n", $1, $2}'
  echo ""
  echo "Per-zone distinct instances:"
  tail -n +2 "$WIDE" | awk -F, '{print $2","$3}' | sort -u \
    | awk -F, '{c[$2]++} END {for (z in c) printf "  %-18s %d\n", z, c[z]}'
  echo ""
  echo "Full timeline in ${WIDE} — send it over for over-provisioning analysis."
  rm -f "$STATE_FILE"
}
trap 'summary; exit 0' SIGINT SIGTERM

while true; do
  POLL_TS=$(date +%s)
  # One gcloud call; basename strips the long machineType/zone URLs.
  ROWS=$(gcloud compute instances list \
    --project "$PROJECT" \
    --filter="name~^gke-${CLUSTER}" \
    --format="csv[no-heading](name,zone.basename(),machineType.basename(),status,creationTimestamp)" \
    2>/dev/null)

  if [ -n "$ROWS" ]; then
    while IFS=, read -r name zone mtype status cts; do
      [ -z "$name" ] && continue
      echo "${POLL_TS},${name},${zone},${mtype},${status},${cts}" >> "$WIDE"
      # Event detection: new instance or status change.
      PREV=$(grep "^${name}=" "$STATE_FILE" 2>/dev/null | cut -d= -f2-)
      if [ -z "$PREV" ]; then
        echo "$(date -u +'%Y-%m-%dT%H:%M:%S') NEW    ${name} ${mtype} ${zone} status=${status} created=${cts}" >> "$EVENTS"
        echo "${name}=${status}" >> "$STATE_FILE"
      elif [ "$PREV" != "$status" ]; then
        echo "$(date -u +'%Y-%m-%dT%H:%M:%S') CHANGE ${name} ${PREV} -> ${status}" >> "$EVENTS"
        grep -v "^${name}=" "$STATE_FILE" > "${STATE_FILE}.tmp" && mv "${STATE_FILE}.tmp" "$STATE_FILE"
        echo "${name}=${status}" >> "$STATE_FILE"
      fi
    done <<< "$ROWS"
  fi
  sleep "$POLL_SECONDS"
done
