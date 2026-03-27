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


RUN_ID=$(date +%Y%m%d-%H%M%S)
if [ -n "$1" ]; then
  RUN_ID+="-${1}"
fi

# BURST_SIZE * TOTAL_BURSTS = Total sandbox claims created
BURST_SIZE=1000
QPS=1000
TOTAL_BURSTS=10
WARMPOOL_SIZE=1000
RUNTIME_CLASS="" # Change to "gvisor" if your cluster supports it

# Update these paths to match your environment
# Clusterloader2 must be cloned or forked from https://github.com/kubernetes/perf-tests
CL2_DIR="${HOME}/perf-tests/clusterloader2"
AGENTS_DIR="${HOME}/agent-sandbox"
TEST_DIR="${AGENTS_DIR}/dev/load-test/test-recipes"
TEST_CONFIG="${TEST_DIR}/rapid-burst-test.yaml"
LOGS_DIR="${TEST_DIR}/tmp/${RUN_ID}"

mkdir -p "$LOGS_DIR"

echo "=== Starting Native CL2 $(($BURST_SIZE*$TOTAL_BURSTS)) Burst Load Test ==="
echo "Burst Size: $BURST_SIZE, QPS: $QPS, Total Bursts: $TOTAL_BURSTS, Warmpool Size: $WARMPOOL_SIZE"

# Create overrides specifying the CL2 parameters
cat <<JSON_EOF > "${LOGS_DIR}/testoverrides.json"
{
  "CL2_QPS": $QPS,
  "CL2_BURST_SIZE": $BURST_SIZE,
  "CL2_TOTAL_BURSTS": $TOTAL_BURSTS,
  "CL2_WARMPOOL_SIZE": $WARMPOOL_SIZE,
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
