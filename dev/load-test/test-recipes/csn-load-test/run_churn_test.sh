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


# *************************************************************************
# CSN churn test orchestrator — the real-world profile that ClusterLoader2
# cannot express: TTL-based turnover (claims created AND expiring concurrently,
# controller-driven shutdownTime expiry) over a rate curve that holds a baseline,
# spikes to a peak, drops to full idle, then recovers — so the cluster reaches
# steady state, SCALES DOWN during idle, then scales back up, which is where
# CSN's cost advantage (and re-suspension behavior) appears.
#
# What this script does: renders manifests/ (envsubst) + applies them, waits on
# the suspension gate (CSN runs), launches monitoring, then drives the churn.
#
# Default profile (PROFILE="5:600,30:180,0:360,5:300", SHUTDOWN_TTL=120s):
#   Phase         Rate   Duration  Population (~rate x TTL)
#   1 Baseline     5/s    600s      ~600 steady
#   2 Peak        30/s    180s      ~3600 transient peak
#   3 Idle         0/s    360s      drains to 0 — CSN re-suspends, nodes scale down
#   4 Recovery     5/s    300s      rebuilds to ~600 steady
#   (driver then bulk-deletes leftover claims)
# Override PROFILE for other shapes (e.g. the longer "mountain" in the README).
#
# Expectations (CSN+ASN): sustained claims <1s; the 30/s peak <5s (ASN bridges
# the ~30s CSN resume gap). CSN-only (ENABLE_ASN_BUFFER=false): sustained <1s,
# brief ~30s tail at the peak ramp (CSN resume cap).
#
# Run scenarios:
#   static:  pre-provision a fixed fleet sized to the CSN run's PEAK RUNNING node
#            count, disable autoscaling + NAP, then no buffers:
#              ENABLE_HPA=false ./run_churn_test.sh static
#   csn:     ENABLE_CSN_BUFFER=true ENABLE_ASN_BUFFER=true ./run_churn_test.sh csn
#   csn-only: ENABLE_CSN_BUFFER=true ./run_churn_test.sh csn-only
#
# QUOTA: default peak ~3600 pods => ~720 active cores at 200m/sandbox. Verify
# CPUS quota for your machine type FIRST — higher-rate profiles or a longer TTL
# scale this up fast and will hit the quota wall that broke the old burst tests.
#
# Measurement is via the GMP/Prometheus pipeline (same metrics the HPA uses);
# this script only generates load + logs node states and population.
# ***************************************************************************

set -e
set -o pipefail

RUN_ID=$(date +%Y%m%d-%H%M%S)
if [ -n "$1" ]; then
  RUN_ID+="-${1}"
fi

# --- Traffic profile: "rate:duration_seconds" segments ("0:N" = idle pause) ---
PROFILE="${PROFILE:-5:600,30:180,0:360,5:300}"
# Per-claim lifetime. Each claim is stamped shutdownTime = now + SHUTDOWN_TTL and
# shutdownPolicy Retain; the controller deletes the sandbox at that time
# (proactive, not GC cascade). Population ~= rate x SHUTDOWN_TTL.
SHUTDOWN_TTL="${SHUTDOWN_TTL:-120}"

# --- Namespace / workload ---
NAMESPACE="${NAMESPACE:-agent-sandbox-churn}"
WARMPOOL_NAME="${WARMPOOL_NAME:-churn-warmpool}"
WARMPOOL_SIZE="${WARMPOOL_SIZE:-300}"
SANDBOX_CPU="${SANDBOX_CPU:-200m}"
SANDBOX_MEMORY="${SANDBOX_MEMORY:-256Mi}"
# Pin NAP to a dense machine type via Custom ComputeClass. Without this, NAP
# packs 200m pods onto small nodes (e2-standard-16 / e2-highcpu), needing ~40%
# more nodes and hitting the node ceiling sooner. nodeSelector on the sandbox
# template propagates to the CSN/ASN buffer nodes too.
COMPUTE_CLASS="${COMPUTE_CLASS:-n2-std-32-churn}"
MACHINE_TYPE="${MACHINE_TYPE:-n2-standard-32}"

# --- HPA ---
ENABLE_HPA="${ENABLE_HPA:-true}"
HPA_MIN_REPLICAS="${HPA_MIN_REPLICAS:-$WARMPOOL_SIZE}"
# 2000: sustained 10/s safety stock (10 x 90s x 2) + spike absorption.
HPA_MAX_REPLICAS="${HPA_MAX_REPLICAS:-2000}"
HPA_TARGET_VALUE="${HPA_TARGET_VALUE:-0.5}"
HPA_METRIC_NAME="${HPA_METRIC_NAME:-prometheus.googleapis.com|agent_sandbox_claim_creation_total|counter}"
HPA_SCALE_UP_PERCENT="${HPA_SCALE_UP_PERCENT:-900}"
HPA_SCALE_DOWN_STABILIZATION="${HPA_SCALE_DOWN_STABILIZATION:-300}"

# --- Buffers (fixed-size; never percentage with HPA on the same warmpool) ---
ENABLE_CSN_BUFFER="${ENABLE_CSN_BUFFER:-false}"
# 600 cores = 3000 pod units: covers spike volume + refill blind spot.
CSN_BUFFER_CORES="${CSN_BUFFER_CORES:-600}"
CSN_INIT_TIME="${CSN_INIT_TIME:-5m}"
ENABLE_ASN_BUFFER="${ENABLE_ASN_BUFFER:-false}"
# ASN bridges the ~30s CSN resume gap: size = peak_rate x 30s x 0.2 cores/pod.
# 300 cores = 1500 pods, covering peaks up to ~50/s for 30s (ample for the
# default 30/s peak). Raising the peak rate needs proportionally more ASN.
ASN_BUFFER_CORES="${ASN_BUFFER_CORES:-300}"

# --- Suspension gate ---
ENABLE_SUSPENSION_GATE="${ENABLE_SUSPENSION_GATE:-true}"
GATE_TIMEOUT_SECONDS="${GATE_TIMEOUT_SECONDS:-1800}"
GATE_POLL_SECONDS="${GATE_POLL_SECONDS:-30}"
MACHINE_CORES="${MACHINE_CORES:-32}"
if [ -z "$MIN_SUSPENDED_NODES" ]; then
  MIN_SUSPENDED_NODES=$(( CSN_BUFFER_CORES * 7 / 10 / MACHINE_CORES ))
  [ "$MIN_SUSPENDED_NODES" -lt 1 ] && MIN_SUSPENDED_NODES=1
fi

# Delete namespace-scoped test resources at the end (claims are always drained).
TEARDOWN="${TEARDOWN:-false}"

# --- Paths ---
# Resolve relative to this script's own location, not an assumed checkout path.
TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE_SCRIPT="${TEST_DIR}/suspension-gate.sh"
DRIVER="${TEST_DIR}/churn-driver.py"
LOGS_DIR="${TEST_DIR}/tmp/${RUN_ID}"

# --- Pre-flight ---
for f in "$DRIVER" "$GATE_SCRIPT"; do
  if [ ! -f "$f" ]; then
    echo "ERROR: missing companion file: $f" >&2
    echo "Copy churn-driver.py and suspension-gate.sh into ${TEST_DIR}." >&2
    exit 1
  fi
done

echo "Verifying Kubernetes cluster connection..."
kubectl cluster-info > /dev/null

if ! python3 -c "import kubernetes" 2>/dev/null; then
  echo "ERROR: python kubernetes client missing. pip3 install kubernetes" >&2
  exit 1
fi

if [ "$ENABLE_CSN_BUFFER" = "true" ] || [ "$ENABLE_ASN_BUFFER" = "true" ]; then
  if ! kubectl api-resources --api-group=autoscaling.x-k8s.io 2>/dev/null | grep -q CapacityBuffer; then
    echo "ERROR: CapacityBuffer API not available." >&2
    exit 1
  fi
fi

SB_WORKERS=$(kubectl get deploy -n agent-sandbox-system -o jsonpath='{range .items[*]}{.spec.template.spec.containers[*].args}{"\n"}{end}' 2>/dev/null \
  | grep -o 'sandbox-concurrent-workers=[0-9]*' | head -1 | cut -d= -f2 || true)
CLAIM_WORKERS=$(kubectl get deploy -n agent-sandbox-system -o jsonpath='{range .items[*]}{.spec.template.spec.containers[*].args}{"\n"}{end}' 2>/dev/null \
  | grep -o 'sandbox-claim-concurrent-workers=[0-9]*' | head -1 | cut -d= -f2 || true)
if [ -z "$SB_WORKERS" ] || [ "${SB_WORKERS:-0}" -lt 100 ] || [ -z "$CLAIM_WORKERS" ] || [ "${CLAIM_WORKERS:-0}" -lt 100 ]; then
  echo "WARNING: --sandbox-concurrent-workers=${SB_WORKERS:-unset} and --sandbox-claim-concurrent-workers=${CLAIM_WORKERS:-unset}." >&2
  echo "         Under high churn, these paths require concurrent workers (recommended >= 100)." >&2
fi

mkdir -p "$LOGS_DIR"

echo ""
echo "=== Realistic Churn Test: ${RUN_ID} ==="
echo "Profile:   ${PROFILE}"
echo "Namespace: ${NAMESPACE}  WarmPool: ${WARMPOOL_NAME} (${WARMPOOL_SIZE})"
echo "HPA:       ${ENABLE_HPA} min=${HPA_MIN_REPLICAS} max=${HPA_MAX_REPLICAS} target=${HPA_TARGET_VALUE}"
echo "CSN:       ${ENABLE_CSN_BUFFER} (${CSN_BUFFER_CORES} cores, init-time ${CSN_INIT_TIME})"
echo "ASN:       ${ENABLE_ASN_BUFFER} (${ASN_BUFFER_CORES} cores)"
echo "Machine:   pinned to ${MACHINE_TYPE} via ComputeClass ${COMPUTE_CLASS}"
echo "Expiry:    controller-native shutdownTime=${SHUTDOWN_TTL}s (Retain); pop ~= rate x ${SHUTDOWN_TTL}"
echo ""
echo "Quota reminder: check regional CPUS quota AND NAP max-CPU cover the peak."
echo "  peak active cores ~= (peak_population + warmpool) x ${SANDBOX_CPU} + buffer cores"
echo ""

# --- Infra setup (kubectl + envsubst-rendered manifests in manifests/) ---
MANIFESTS="${TEST_DIR}/manifests"
if ! command -v envsubst >/dev/null 2>&1; then
  echo "ERROR: envsubst not found (install gettext: 'apt-get install gettext-base')." >&2
  exit 1
fi
# Export every var the manifests reference so envsubst can substitute them.
export NAMESPACE COMPUTE_CLASS MACHINE_TYPE WARMPOOL_NAME WARMPOOL_SIZE \
  SANDBOX_CPU SANDBOX_MEMORY HPA_MIN_REPLICAS HPA_MAX_REPLICAS HPA_METRIC_NAME \
  HPA_TARGET_VALUE HPA_SCALE_UP_PERCENT HPA_SCALE_DOWN_STABILIZATION \
  ASN_BUFFER_CORES CSN_BUFFER_CORES CSN_INIT_TIME

apply() { envsubst < "${MANIFESTS}/$1" | kubectl apply -f - ; }

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
apply computeclass.yaml
apply sandbox-template.yaml
apply warmpool.yaml
if [ "$ENABLE_HPA" = "true" ]; then apply hpa.yaml; fi
if [ "$ENABLE_ASN_BUFFER" = "true" ]; then apply asn-buffer.yaml; fi
if [ "$ENABLE_CSN_BUFFER" = "true" ]; then apply csn-buffer.yaml; fi

# --- Wait for warmpool ready ---
echo ""
echo "Waiting for warmpool ${WARMPOOL_NAME} to reach ${WARMPOOL_SIZE} ready replicas..."
DEADLINE=$(( $(date +%s) + 900 ))
while true; do
  READY=$(kubectl get sandboxwarmpool "$WARMPOOL_NAME" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
  echo "  readyReplicas=${READY:-0}/${WARMPOOL_SIZE}"
  [ "${READY:-0}" -ge "$WARMPOOL_SIZE" ] && break
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    echo "ERROR: warmpool not ready within 15m." >&2
    exit 1
  fi
  sleep 15
done

# --- Suspension gate ---
if [ "$ENABLE_CSN_BUFFER" = "true" ] && [ "$ENABLE_SUSPENSION_GATE" = "true" ]; then
  echo ""
  echo "Suspension gate: waiting for >= ${MIN_SUSPENDED_NODES} SUSPENDED nodes"
  echo "(NAP provision ~3-5m + init-time ${CSN_INIT_TIME} active + suspend ~1-2m)"
  timeout "$GATE_TIMEOUT_SECONDS" bash "$GATE_SCRIPT" "$MIN_SUSPENDED_NODES" "$GATE_POLL_SECONDS" || {
    echo "ERROR: suspension gate timed out — CSN nodes never suspended." >&2
    echo "Starting load now would test ACTIVE capacity, not standby. Aborting." >&2
    exit 1
  }
fi

# --- Background node-state logger ---
NODE_LOG="${LOGS_DIR}/node-states.csv"
echo "timestamp,total_nodes,suspended,resumed_prev_suspended" > "$NODE_LOG"
(
  while true; do
    STATES=$(kubectl get nodes -o custom-columns='S:.status.conditions[?(@.type=="Suspended")].status' --no-headers 2>/dev/null)
    TOTAL=$(echo "$STATES" | grep -c .)
    SUSP=$(echo "$STATES" | grep -c True || true)
    RESUMED=$(echo "$STATES" | grep -c False || true)
    echo "$(date +%s),${TOTAL},${SUSP},${RESUMED}" >> "$NODE_LOG"
    sleep 30
  done
) &
NODE_LOGGER_PID=$!

# --- Launch the full monitoring bundle into the SAME run folder ---
# Co-locates warmpool_ready/hpa/pods_status/nodes/autoscaler/instances logs with
# churn-stats.csv + node-states.csv. No separate terminal / RUN_ID coordination.
MONITOR_PID=""
if [ -f "${TEST_DIR}/monitor-all.sh" ]; then
  LOG_DIR="$LOGS_DIR" RUN_ID="$RUN_ID" \
    MONITOR_NAMESPACE="$NAMESPACE" WARMPOOL="$WARMPOOL_NAME" \
    bash "${TEST_DIR}/monitor-all.sh" > "${LOGS_DIR}/monitor-all.out" 2>&1 &
  MONITOR_PID=$!
  echo "Monitoring bundle started (PID ${MONITOR_PID}) → ${LOGS_DIR}"
fi

cleanup() {
  [ -n "$NODE_LOGGER_PID" ] && kill "$NODE_LOGGER_PID" 2>/dev/null || true
  [ -n "$MONITOR_PID" ] && kill "$MONITOR_PID" 2>/dev/null || true
}
trap cleanup EXIT

# --- Run the churn driver ---
echo ""
echo "Starting churn driver (logs: ${LOGS_DIR}/churn-driver.log)..."
python3 "$DRIVER" \
  --namespace "$NAMESPACE" \
  --warmpool "$WARMPOOL_NAME" \
  --profile "$PROFILE" \
  --shutdown-ttl "$SHUTDOWN_TTL" \
  --run-id "$RUN_ID" \
  --stats-file "${LOGS_DIR}/churn-stats.csv" \
  2>&1 | tee "${LOGS_DIR}/churn-driver.log"

# --- Teardown (optional) ---
if [ "$TEARDOWN" = "true" ]; then
  echo "Tearing down test resources in ${NAMESPACE}..."
  kubectl delete hpa "${WARMPOOL_NAME}-hpa" -n "$NAMESPACE" --ignore-not-found
  kubectl delete capacitybuffer churn-csn-buffer churn-asn-buffer -n "$NAMESPACE" --ignore-not-found
  kubectl delete sandboxwarmpool "$WARMPOOL_NAME" -n "$NAMESPACE" --ignore-not-found
  kubectl delete sandboxtemplate churn-template -n "$NAMESPACE" --ignore-not-found
  kubectl delete computeclass "$COMPUTE_CLASS" --ignore-not-found
else
  echo "Resources kept (TEARDOWN=false). Cleanup later with:"
  echo "  kubectl delete hpa ${WARMPOOL_NAME}-hpa -n ${NAMESPACE}"
  echo "  kubectl delete capacitybuffer churn-csn-buffer churn-asn-buffer -n ${NAMESPACE}"
  echo "  kubectl delete sandboxwarmpool ${WARMPOOL_NAME} -n ${NAMESPACE}"
  echo "  kubectl delete sandboxtemplate churn-template -n ${NAMESPACE}"
  echo "  kubectl delete computeclass ${COMPUTE_CLASS}"
fi

echo ""
echo "Artifacts: ${LOGS_DIR}"
echo "  churn-stats.csv   population/rate per 10s — overlay on latency series"
echo "  node-states.csv   suspended/resumed counts per 30s"
echo ""
echo "Analysis (Cloud Monitoring PromQL, 1m windows):"
echo "  histogram_quantile(0.99, sum(rate(agent_sandbox_claim_startup_latency_ms_bucket[1m])) by (le, launch_type))"
echo "  sum(rate(agent_sandbox_claim_creation_total{launch_type=\"cold\"}[1m]))"
echo ""
echo "Per-phase expectations:"
echo "  ramp/steady phases: P99 < 1s | peak ramp: < 5s with ASN, ~30s tail CSN-only (resume cap)"
echo "  idle tail must show re-suspension (node-states total falls); idle latency must match ramp-up."
