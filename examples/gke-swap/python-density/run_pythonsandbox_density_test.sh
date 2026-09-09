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

set -eo pipefail

# Directory paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Resolve KUBECONFIG: Use existing KUBECONFIG or fall back to ~/.kube/config directly
export KUBECONFIG="${KUBECONFIG:-"${HOME}/.kube/config"}"

# Define benchmark configuration defaults (overrideable via environment variables)
# POOLS: Target GKE node pools (e.g., "lssd-swap-pool baseline-pool")
POOLS="${POOLS:-lssd-swap-pool baseline-pool}"

# DENSITIES: Target sandbox density levels per sweep
DENSITIES="${DENSITIES:-20 40 60 100 120 140}"

# RUNTIME_CLASS: Target container runtime class (e.g. "gvisor" or empty for default runc)
RUNTIME_CLASS="${RUNTIME_CLASS:-}"

# Clean up previous benchmark artifacts before starting new runs
rm -rf "${SCRIPT_DIR}/artifacts"

# Initialize timestamped artifact output directory
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
ARTIFACT_DIR="${SCRIPT_DIR}/artifacts/run_${TIMESTAMP}"
mkdir -p "${ARTIFACT_DIR}"

# Log helper function with timestamp
log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

cd "${REPO_ROOT}"

# Step 1: Pre-stage MovieLens dataset ONCE on all target nodes under /tmp/movielens
# Uses an ephemeral Alpine pod mounting host root to download/extract ratings.csv directly onto host disk
log "=== Pre-staging ML-20M dataset on nodes (/tmp/movielens/ratings.csv) ==="
for pool in ${POOLS}; do
    NODE=$(kubectl get nodes -l "cloud.google.com/gke-nodepool=${pool}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -n "${NODE}" ]; then
        log "Pre-staging ML-20M dataset on node ${NODE}..."
        if ! kubectl run "prestager-${pool}" --image=alpine --restart=Never --overrides="{
          \"spec\": {
            \"nodeName\": \"${NODE}\",
            \"containers\": [{
              \"name\": \"prestage\",
              \"image\": \"alpine\",
              \"command\": [\"sh\", \"-c\", \"mkdir -p /host/tmp/movielens && if [ ! -f /host/tmp/movielens/ratings.csv ]; then if [ -f /host/mnt/stateful_partition/movielens/ratings.csv ]; then cp /host/mnt/stateful_partition/movielens/ratings.csv /host/tmp/movielens/ratings.csv; else wget -q -O /host/tmp/movielens/ml-20m.zip https://files.grouplens.org/datasets/movielens/ml-20m.zip && unzip -q -o /host/tmp/movielens/ml-20m.zip -d /tmp/ && mv /tmp/ml-20m/ratings.csv /host/tmp/movielens/ratings.csv && rm -f /host/tmp/movielens/ml-20m.zip; fi; fi && ls -lh /host/tmp/movielens/ratings.csv\"],
              \"volumeMounts\": [{\"name\": \"h\", \"mountPath\": \"/host\"}]
            }],
            \"volumes\": [{\"name\": \"h\", \"hostPath\": {\"path\": \"/\"}}]
          }
        }" >/dev/null 2>&1; then
            log "ERROR: Failed to create prestager pod for node ${NODE}"
            exit 1
        fi
        
        # Wait for prestaging pod to finish downloading without attaching stdin
        if ! kubectl wait --for=jsonpath='{.status.phase}'=Succeeded "pod/prestager-${pool}" --timeout=300s >/dev/null 2>&1; then
            log "ERROR: Failed to prestage MovieLens dataset on node ${NODE}"
            kubectl logs "pod/prestager-${pool}" 2>&1 || true
            kubectl delete "pod/prestager-${pool}" --grace-period=0 --force >/dev/null 2>&1 || true
            exit 1
        fi
        kubectl delete "pod/prestager-${pool}" --grace-period=0 --force >/dev/null 2>&1 || true
    fi
done

# Step 2: Execute multi-density benchmark matrix across target node pools
log "=== Starting MovieLens High-Density Benchmark Sweeps ==="
log "Pools: ${POOLS}"
log "Densities: ${DENSITIES}"
log "Artifact Directory: ${ARTIFACT_DIR}"

FAILED_SWEEPS=0

for pool in ${POOLS}; do
    for density in ${DENSITIES}; do
        log "----------------------------------------------------------------------"
        log "Starting sweep: Pool=${pool}, Density=${density}"
        log "----------------------------------------------------------------------"

        # Create sweep-specific output directory for metrics and test logs
        SWEEP_DIR="${ARTIFACT_DIR}/${pool}/${density}"
        mkdir -p "${SWEEP_DIR}"

        # Clean up any lingering test namespaces (perf-py-*), PVCs, and PVs, stripping finalizers
        log "Purging any lingering test namespaces, PVCs, and PVs (perf-py-*) and stripping finalizers..."
        for pv in $(kubectl get pv -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -E '^movielens-pv-perf-py-' || true); do
            kubectl patch pv "${pv}" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
            kubectl delete pv "${pv}" --force --grace-period=0 2>/dev/null || true
        done
        for ns in $(kubectl get ns -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -E '^perf-py-' || true); do
            for pvc in $(kubectl get pvc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
                kubectl patch pvc "${pvc}" -n "${ns}" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
            done
            kubectl get sandboxes -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | xargs -r -L1 -I {} kubectl patch sandbox {} -n "${ns}" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
            kubectl patch ns "${ns}" -p '{"spec":{"finalizers":[]}}' --type=merge 2>/dev/null || true
            kubectl delete ns "${ns}" --force --grace-period=0 2>/dev/null || true
        done

        # Wait for all test namespaces to be fully purged from the cluster before proceeding
        log "Waiting for old test namespaces and pods to be completely removed..."
        while kubectl get ns 2>/dev/null | grep -q -E '^perf-py-'; do
            sleep 2
        done
        log "Cluster namespace purge complete. Node is clean."

        # Identify target node name matching current GKE node pool
        NODE_NAME=$(kubectl get nodes -l "cloud.google.com/gke-nodepool=${pool}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [ -z "${NODE_NAME}" ]; then
            log "No node found for pool ${pool}"
            exit 1
        fi

        # Flush host node Page Cache to reset node RAM to clean baseline
        if [ -n "${NODE_NAME}" ]; then
            log "Flushing Page Cache on node ${NODE_NAME}..."
            kubectl run --rm "cache-dropper-${pool}" --image=alpine --restart=Never --overrides="{
              \"spec\": {
                \"nodeName\": \"${NODE_NAME}\",
                \"hostPID\": true,
                \"containers\": [{
                  \"name\": \"drop-cache\",
                  \"image\": \"alpine\",
                  \"securityContext\": {\"privileged\": true},
                  \"command\": [\"sh\", \"-c\", \"echo 3 > /host/proc/sys/vm/drop_caches\"],
                  \"volumeMounts\": [{\"name\": \"proc\", \"mountPath\": \"/host/proc\"}]
                }],
                \"volumes\": [{\"name\": \"proc\", \"hostPath\": {\"path\": \"/proc\"}}]
              }
            }" >/dev/null 2>&1 || true
        fi

        # Find host cAdvisor telemetry monitor pod for node metrics aggregation
        MONITOR_POD=$(kubectl get pods -n default -l name=kubelet-cadvisor-monitor --field-selector="spec.nodeName=${NODE_NAME}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        TEST_START_TIME=$(date +%s)

        # Start real-time telemetry streaming in background to avoid log buffer rollover
        TELEMETRY_PID=""
        if [ -n "${MONITOR_POD}" ]; then
            log "Starting background telemetry stream from ${MONITOR_POD}..."
            kubectl logs -f "${MONITOR_POD}" -n default 2>/dev/null > "${SWEEP_DIR}/raw_telemetry.log" &
            TELEMETRY_PID=$!
        fi

        # Run Go E2E density test suite targeting TestPythonSandboxDensity
        if ! POOLS="${pool}" DENSITIES="${density}" \
           BENCHMARK_SCRIPT_PATH="${REPO_ROOT}/test/e2e/extensions/python_workload.py" \
           ARTIFACTS="${SWEEP_DIR}" \
           ARTIFACT_DIR="${SWEEP_DIR}" \
           go test -v -timeout 45m ./test/e2e/extensions/ -run ^TestPythonSandboxDensity$ \
             -args -kubeconfig="${KUBECONFIG}" -run-perf-load-test -density="${density}" \
             -node-name="${NODE_NAME}" -runtime-class-name="${RUNTIME_CLASS}" 2>&1 | tee "${SWEEP_DIR}/test.log"; then
            log "ERROR: Sweep failed for pool=${pool}, density=${density}"
            FAILED_SWEEPS=$(( FAILED_SWEEPS + 1 ))
        fi

        TEST_END_TIME=$(date +%s)

        # Stop background telemetry streaming
        if [ -n "${TELEMETRY_PID}" ]; then
            kill "${TELEMETRY_PID}" 2>/dev/null || true
            wait "${TELEMETRY_PID}" 2>/dev/null || true
        fi

        # Process streamed telemetry CSV and parse peak RAM, swap, and PSI metrics
        if [ -n "${MONITOR_POD}" ] && [ -f "${SWEEP_DIR}/raw_telemetry.log" ]; then
            log "Processing telemetry stream for time window ${TEST_START_TIME} to ${TEST_END_TIME}..."
            awk -F, -v start="${TEST_START_TIME}" -v end="${TEST_END_TIME}" '$4 >= start && $4 <= end' "${SWEEP_DIR}/raw_telemetry.log" > "${SWEEP_DIR}/resource_profile.csv" || true
            rm -f "${SWEEP_DIR}/raw_telemetry.log"
            
            if ! python3 "${SCRIPT_DIR}/parse_telemetry.py" \
              "${SWEEP_DIR}/resource_profile.csv" \
              "${SWEEP_DIR}/TestPythonSandboxDensity/density_metrics.json"; then
                log "ERROR: Telemetry parser failed for sweep pool=${pool}, density=${density}"
                FAILED_SWEEPS=$(( FAILED_SWEEPS + 1 ))
            fi
        fi

        log "Completed sweep: Pool=${pool}, Density=${density}"
    done
done

log "=== All MovieLens Density Sweeps Completed ==="
log "Results saved in: ${ARTIFACT_DIR}"

if [ "${FAILED_SWEEPS}" -gt 0 ]; then
    log "ERROR: ${FAILED_SWEEPS} benchmark sweep(s) failed."
    exit 1
fi
