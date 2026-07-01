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
# Tuned all-in-one monitor for CSN load tests.
#
# Usage:
#   MONITOR_NAMESPACE=agent-sandbox-churn WARMPOOL=churn-warmpool \
#   PROJECT=gke-ai-eco-dev CLUSTER=csn-hpa-sandbox-burst ./monitor-all.sh
# ***************************************************************************

set -o pipefail

if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq CLI not found. Please install jq (e.g., 'apt-get install jq')." >&2
  exit 1
fi

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="${RUN_ID:-$(date +%Y%m%d-%H%M%S)}"
# Default to the SAME tmp/<RUN_ID> folder the runner + churn-driver write to, so
# every artifact for a run lives in one place. Override with LOG_DIR=... .
LOG_DIR="${LOG_DIR:-${TEST_DIR}/tmp/${RUN_ID}}"
mkdir -p "$LOG_DIR"

MONITOR_NAMESPACE="${MONITOR_NAMESPACE:-agent-sandbox-churn}"
WARMPOOL="${WARMPOOL:-churn-warmpool}"
PROJECT="${PROJECT:-gke-ai-eco-dev}"
CLUSTER="${CLUSTER:-csn-hpa-sandbox-burst}"

echo "=== CSN Load Test Monitoring — run ${RUN_ID} ==="
echo "Logs: ${LOG_DIR}"
PIDS=()

# 1. *** WARMPOOL ready buffer *** — the most important signal.
#    ready = pods available for instant adoption. When this hits ~0 during a
#    spike, claims adopt un-ready sandboxes and wait => warm-launch latency.
#    gap (replicas - ready) = sandboxes still provisioning (refill in flight).
(
  echo "ts,spec_replicas,status_replicas,ready_replicas,provisioning_gap"
  while true; do
    JSON=$(kubectl get sandboxwarmpool "$WARMPOOL" -n "$MONITOR_NAMESPACE" -o json 2>/dev/null)
    if [ -n "$JSON" ]; then
      SPEC=$(echo "$JSON" | jq -r '.spec.replicas // 0')
      REPL=$(echo "$JSON" | jq -r '.status.replicas // 0')
      RDY=$(echo "$JSON" | jq -r '.status.readyReplicas // 0')
      echo "$(date +%s),${SPEC},${REPL},${RDY},$((REPL - RDY))"
    fi
    sleep 5
  done
) > "${LOG_DIR}/warmpool_ready.csv" 2>&1 &
PIDS+=($!)

# 2. HPA: current/desired replicas + the external metric value driving it.
(
  echo "ts,current_replicas,desired_replicas,current_metric,target_metric"
  while true; do
    JSON=$(kubectl get hpa "${WARMPOOL}-hpa" -n "$MONITOR_NAMESPACE" -o json 2>/dev/null)
    if [ -n "$JSON" ]; then
      echo "$JSON" | jq -r --arg ts "$(date +%s)" '[$ts, (.status.currentReplicas // 0), (.status.desiredReplicas // 0), (.status.currentMetrics[0].external.current.value // "na"), (.spec.metrics[0].external.target.value // "na")] | @csv'
    fi
    sleep 5
  done
) > "${LOG_DIR}/hpa.csv" 2>&1 &
PIDS+=($!)

# 3. CapacityBuffer status (CSN + ASN): desired vs provisioned replicas.
stdbuf -oL kubectl get capacitybuffer -n "$MONITOR_NAMESPACE" -w \
  > "${LOG_DIR}/capacity_buffer.log" 2>&1 &
PIDS+=($!)

# 4. Workload pods: pending is the red flag (capacity actually exhausted).
(
  echo "ts,workload_pending,workload_running,workload_terminating,standby_pending,standby_running"
  while true; do
    ALL=$(kubectl get pods -n "$MONITOR_NAMESPACE" --no-headers 2>/dev/null)
    WP=$(echo "$ALL" | grep -v "standby-capacity" | grep -ci pending)
    WR=$(echo "$ALL" | grep -v "standby-capacity" | grep -ci running)
    # Terminating zombie pods — if this stays high under churn, the 30s grace /
    # deletion backlog is squatting capacity (the run_190 failure mode).
    WT=$(echo "$ALL" | grep -v "standby-capacity" | grep -ci terminating)
    SP=$(echo "$ALL" | grep "standby-capacity" | grep -ci pending)
    SR=$(echo "$ALL" | grep "standby-capacity" | grep -ci running)
    echo "$(date +%s),${WP},${WR},${WT},${SP},${SR}"
    sleep 5
  done
) > "${LOG_DIR}/pods_status.csv" 2>&1 &
PIDS+=($!)

# 5. Nodes by pool / machine type / Ready + Suspended condition.
(
  while true; do
    echo "=== NODE CHECK: $(date -u +'%Y-%m-%dT%H:%M:%S') ==="
    kubectl get nodes -o 'custom-columns=NAME:.metadata.name,POOL:.metadata.labels.cloud\.google\.com/gke-nodepool,TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,READY:.status.conditions[?(@.type=="Ready")].status,SUSPENDED:.status.conditions[?(@.type=="Suspended")].status' 2>/dev/null
    echo ""
    sleep 10
  done
) > "${LOG_DIR}/nodes.log" 2>&1 &
PIDS+=($!)

# 6. Cluster Autoscaler status + events (quota warnings show up here).
(
  while true; do
    echo "=== AUTOSCALER: $(date -u +'%Y-%m-%dT%H:%M:%S') ==="
    kubectl get configmap cluster-autoscaler-status -n kube-system -o jsonpath='{.data.status}' 2>/dev/null
    echo -e "\n"
    sleep 15
  done
) > "${LOG_DIR}/autoscaler_status.log" 2>&1 &
PIDS+=($!)

stdbuf -oL kubectl get events -A --watch --field-selector source=cluster-autoscaler \
  -o jsonpath='{.metadata.creationTimestamp}{"\t"}{.involvedObject.kind}{"\t"}{.reason}{"\t"}{.message}{"\n"}' \
  > "${LOG_DIR}/autoscaler_events.log" 2>&1 &
PIDS+=($!)

stdbuf -oL kubectl get events -n "$MONITOR_NAMESPACE" --watch \
  --field-selector involvedObject.kind=HorizontalPodAutoscaler \
  -o jsonpath='{.metadata.creationTimestamp}{"\t"}{.reason}{"\t"}{.message}{"\n"}' \
  > "${LOG_DIR}/hpa_events.log" 2>&1 &
PIDS+=($!)

# 7. Instance tracker (machine types, zones, over-provisioning) — separate script.
if [ -f "${TEST_DIR}/monitor-instances.sh" ]; then
  RUN_ID="$RUN_ID" LOG_DIR="$LOG_DIR" PROJECT="$PROJECT" CLUSTER="$CLUSTER" \
    bash "${TEST_DIR}/monitor-instances.sh" > "${LOG_DIR}/instances-monitor.out" 2>&1 &
  PIDS+=($!)
fi

echo "Started ${#PIDS[@]} monitors. Key file to watch live:"
echo "  tail -f ${LOG_DIR}/warmpool_ready.csv   # ready_replicas -> 0 == latency cause"
trap 'echo "Stopping monitors..."; kill "${PIDS[@]}" 2>/dev/null; echo "Logs in ${LOG_DIR}"; exit 0' SIGINT SIGTERM EXIT
while true; do sleep 1; done
