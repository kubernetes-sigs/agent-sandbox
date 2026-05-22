#!/bin/bash
set -e
set -o pipefail

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


RUN_ID=$(date +%Y%m%d-%H%M%S)
if [ -n "$1" ]; then
  RUN_ID+="-${1}"
fi

# Configuration defaults (overridable via environment variables)
BURST_SIZE="${BURST_SIZE:-1000}"
QPS="${QPS:-1000}"
TOTAL_BURSTS="${TOTAL_BURSTS:-10}"
WARMPOOL_SIZE="${WARMPOOL_SIZE:-1000}"
RUNTIME_CLASS="${RUNTIME_CLASS:-}" # Change to "gvisor" if your cluster supports it

# Optional autoscaling & capacity buffer extensions
ENABLE_HPA="${ENABLE_HPA:-false}"
HPA_MIN_REPLICAS="${HPA_MIN_REPLICAS:-1000}"
HPA_MAX_REPLICAS="${HPA_MAX_REPLICAS:-2000}"
HPA_TARGET_VALUE="${HPA_TARGET_VALUE:-0.5}"
HPA_METRIC_NAME="${HPA_METRIC_NAME:-prometheus.googleapis.com|agent_sandbox_claim_creation_total|counter}"

ENABLE_CAPACITY_BUFFER="${ENABLE_CAPACITY_BUFFER:-false}"
BUFFER_PERCENTAGE="${BUFFER_PERCENTAGE:-200}"
PROVISIONING_STRATEGY="${PROVISIONING_STRATEGY:-buffer.gke.io/standby-capacity}"
CAPACITY_BUFFER_PAUSE_DURATION="${CAPACITY_BUFFER_PAUSE_DURATION:-5m}"

# Update these paths to match your environment
# Clusterloader2 must be cloned or forked from https://github.com/kubernetes/perf-tests
CL2_DIR="${HOME}/perf-tests/clusterloader2"
AGENTS_DIR="${HOME}/agent-sandbox"
TEST_DIR="${AGENTS_DIR}/dev/load-test/test-recipes"
TEST_CONFIG="${TEST_DIR}/rapid-burst-test.yaml"
LOGS_DIR="${TEST_DIR}/tmp/${RUN_ID}"

# Pre-flight checks
echo "Verifying connection to Kubernetes cluster..."
if ! kubectl cluster-info &> /dev/null; then
  echo "ERROR: Kubernetes cluster is unreachable. Please check your kubeconfig, context, or network connection." >&2
  exit 1
fi

if [ "$ENABLE_CAPACITY_BUFFER" = "true" ]; then
  echo "Checking if CapacityBuffer API is available in target cluster..."
  if ! kubectl api-resources --api-group=autoscaling.x-k8s.io | grep -q CapacityBuffer; then
    echo "ERROR: GKE CapacityBuffer API (autoscaling.x-k8s.io) is not available in the cluster."
    echo "Ensure GKE Node Auto-provisioning / CapacityBuffer feature is enabled on the target cluster."
    exit 1
  fi
fi
if [ "$ENABLE_HPA" = "true" ]; then
  if [ "$WARMPOOL_SIZE" -lt "$HPA_MIN_REPLICAS" ] || [ "$WARMPOOL_SIZE" -gt "$HPA_MAX_REPLICAS" ]; then
    echo "ERROR: invalid HPA configuration: WARMPOOL_SIZE ($WARMPOOL_SIZE) must be within HPA replica bounds [$HPA_MIN_REPLICAS, $HPA_MAX_REPLICAS] when ENABLE_HPA=true." >&2
    exit 1
  fi
fi

mkdir -p "$LOGS_DIR"

echo "=== Starting Native CL2 $(($BURST_SIZE*$TOTAL_BURSTS)) Burst Load Test ==="
echo "Burst Size: $BURST_SIZE, QPS: $QPS, Total Bursts: $TOTAL_BURSTS, Warmpool Size: $WARMPOOL_SIZE"
echo "HPA Enabled: $ENABLE_HPA, Min Replicas: $HPA_MIN_REPLICAS, Max Replicas: $HPA_MAX_REPLICAS, Target Rate: $HPA_TARGET_VALUE"
echo "CapacityBuffer Enabled: $ENABLE_CAPACITY_BUFFER, Buffer Percentage: $BUFFER_PERCENTAGE%, Strategy: $PROVISIONING_STRATEGY, Pause: $CAPACITY_BUFFER_PAUSE_DURATION"

# Create overrides specifying the CL2 parameters
cat <<JSON_EOF > "${LOGS_DIR}/testoverrides.json"
{
  "CL2_QPS": $QPS,
  "CL2_BURST_SIZE": $BURST_SIZE,
  "CL2_TOTAL_BURSTS": $TOTAL_BURSTS,
  "CL2_WARMPOOL_SIZE": $WARMPOOL_SIZE,
  "CL2_ENABLE_HPA": "$ENABLE_HPA",
  "CL2_HPA_MIN_REPLICAS": $HPA_MIN_REPLICAS,
  "CL2_HPA_MAX_REPLICAS": $HPA_MAX_REPLICAS,
  "CL2_HPA_TARGET_VALUE": "$HPA_TARGET_VALUE",
  "CL2_HPA_METRIC_NAME": "$HPA_METRIC_NAME",
  "CL2_ENABLE_CAPACITY_BUFFER": "$ENABLE_CAPACITY_BUFFER",
  "CL2_BUFFER_PERCENTAGE": $BUFFER_PERCENTAGE,
  "CL2_PROVISIONING_STRATEGY": "$PROVISIONING_STRATEGY",
  "CL2_CAPACITY_BUFFER_PAUSE": "$CAPACITY_BUFFER_PAUSE_DURATION",
  "CL2_TEMPLATE_DIR": "$TEST_DIR",
  "CL2_RUNTIME_CLASS": "$RUNTIME_CLASS"
}
JSON_EOF

# Execute using the cluster loader2 test
cd "$CL2_DIR"
go run cmd/clusterloader.go \
  --enable-prometheus-server=true \
  --kubeconfig=$HOME/.kube/config \
  --prometheus-additional-monitors-path="${TEST_DIR}/monitor" \
  --provider=gke \
  --report-dir="${LOGS_DIR}" \
  --testconfig="${TEST_CONFIG}" \
  --testoverrides="${LOGS_DIR}/testoverrides.json" \
  --v=2 \
  2>&1 | tee "${LOGS_DIR}/clusterloader2-${RUN_ID}.log"
