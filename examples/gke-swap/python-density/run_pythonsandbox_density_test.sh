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
        kubectl run --rm "prestager-${pool}" --image=alpine --restart=Never --overrides="{
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
        }" 2>/dev/null || true
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

        # Clean up any lingering test namespaces (perf-py-*) and strip finalizers from prior failed runs
        log "Purging any lingering test namespaces (perf-py-*) and stripping finalizers..."
        for ns in $(kubectl get ns -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -E '^perf-py-' || true); do
            kubectl get sandboxes -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | xargs -r -L1 -I {} kubectl patch sandbox {} -n "${ns}" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
            kubectl patch ns "${ns}" -p '{"spec":{"finalizers":[]}}' --type=merge 2>/dev/null || true
            kubectl delete ns "${ns}" --force --grace-period=0 2>/dev/null || true
        done

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
        TEST_DURATION=$(( TEST_END_TIME - TEST_START_TIME + 60 ))

        # Extract host node cAdvisor telemetry CSV and parse peak RAM, swap, and PSI metrics
        if [ -n "${MONITOR_POD}" ]; then
            log "Pulling telemetry logs from ${MONITOR_POD}..."
            kubectl logs "${MONITOR_POD}" -n default --since="${TEST_DURATION}s" 2>/dev/null | \
              awk -F, -v start="${TEST_START_TIME}" -v end="${TEST_END_TIME}" '$4 >= start && $4 <= end' > "${SWEEP_DIR}/resource_profile.csv" || true
            
            python3 -c "
import csv, json, os

csv_path = '${SWEEP_DIR}/resource_profile.csv'
json_path = '${SWEEP_DIR}/TestPythonSandboxDensity/density_metrics.json'

if os.path.exists(csv_path) and os.path.exists(json_path):
    metrics_min, metrics_max = {}, {}
    with open(csv_path, 'r') as f:
        reader = csv.reader(f)
        for row in reader:
            if len(row) < 3: continue
            m = row[1].strip()
            try: v = float(row[2].strip())
            except ValueError: continue
            if m not in metrics_min:
                metrics_min[m], metrics_max[m] = v, v
            else:
                if v < metrics_min[m]: metrics_min[m] = v
                if v > metrics_max[m]: metrics_max[m] = v

    ram_max = round(metrics_max.get('node_memory_working_set_bytes', 0.0)/(1024**3), 2)
    ram_delta = round((metrics_max.get('node_memory_working_set_bytes', 0.0) - metrics_min.get('node_memory_working_set_bytes', 0.0))/(1024**3), 2)
    swap_delta = round((metrics_max.get('host_swap_used_bytes', 0.0) - metrics_min.get('host_swap_used_bytes', 0.0))/(1024**3), 2)

    node_telemetry = {
        'peak_node_ram_gb': ram_max,
        'net_ram_added_gb': max(ram_delta, 0.0),
        'net_swap_added_gb': max(swap_delta, 0.0),
        'kubelet_memory_mb': round(metrics_max.get('kubelet_memory', 0.0)/(1024**2), 1),
        'containerd_memory_mb': round(metrics_max.get('runtime_memory', 0.0)/(1024**2), 1),
        'mem_psi_seconds': round(metrics_max.get('host_mem_psi_waiting_seconds_total', 0.0) - metrics_min.get('host_mem_psi_waiting_seconds_total', 0.0), 4),
        'io_psi_seconds': round(metrics_max.get('host_io_psi_waiting_seconds_total', 0.0) - metrics_min.get('host_io_psi_waiting_seconds_total', 0.0), 4),
        'cpu_psi_seconds': round(metrics_max.get('host_cpu_psi_waiting_seconds_total', 0.0) - metrics_min.get('host_cpu_psi_waiting_seconds_total', 0.0), 4),
    }

    with open(json_path, 'r') as f: data = json.load(f)
    data['node_telemetry_peaks'] = node_telemetry
    with open(json_path, 'w') as f: json.dump(data, f, indent=2)
" || true
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
