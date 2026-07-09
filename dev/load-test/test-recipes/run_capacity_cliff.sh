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
STEP_SIZE="${STEP_SIZE:-1000}"
TOTAL_STEPS="${TOTAL_STEPS:-20}"
QPS="${QPS:-100}"
NAMESPACES="${NAMESPACES:-1}"
HOLD_DURATION="${HOLD_DURATION:-2m}"
CONVERGENCE_TIMEOUT="${CONVERGENCE_TIMEOUT:-30m}"
KWOK_NODES="${KWOK_NODES:-false}"
PROVIDER="${PROVIDER:-gke}"
# Exported so the pre-flight kubectl checks below target the same cluster as
# the clusterloader2 run (which receives it via --kubeconfig).
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
# Must be pullable from the cluster's nodes. Private GKE nodes without Cloud
# NAT cannot reach registry.k8s.io — use a gcr.io/pkg.dev-hosted image there.
SANDBOX_IMAGE="${SANDBOX_IMAGE:-registry.k8s.io/pause:3.10}"
# Set to true to apply the SandboxTemplate-shaped NetworkPolicy to sandbox
# pods (production configuration); false measures the unpoliced best case.
NETWORK_POLICY="${NETWORK_POLICY:-false}"

# Prometheus OOM guards. CL2 sets the Prometheus container's memory REQUEST
# AND LIMIT to (MEMORY_LIMIT_FACTOR)Gi x (1 + nodes/1000) — the default factor
# of 2 means a hard 2Gi cap on clusters under 1k nodes, which OOMs long before
# the sandbox counts this test targets. Size the factor to the pod count you
# are ramping to (rule of thumb: ~2Gi base + ~1Gi per 10k pods, then round up
# generously — the limit is a hard cap) and pin Prometheus to a node that can
# hold it via PROMETHEUS_NODE_SELECTOR (YAML fragment, e.g.
# "cloud.google.com/gke-nodepool: my-big-pool").
# Kube-proxy scraping is one target per node and useless to this test — off.
# PROMETHEUS_SLOW_APISERVER=true drops the apiserver scrape from 5s to 30s,
# cutting Prometheus memory/CPU substantially on large clusters.
PROMETHEUS_MEMORY_LIMIT_FACTOR="${PROMETHEUS_MEMORY_LIMIT_FACTOR:-2}"
PROMETHEUS_NODE_SELECTOR="${PROMETHEUS_NODE_SELECTOR:-}"
PROMETHEUS_SLOW_APISERVER="${PROMETHEUS_SLOW_APISERVER:-false}"
SCRAPE_KUBE_PROXY="${SCRAPE_KUBE_PROXY:-false}"

# kind needs three deviations from CL2's GCE-oriented Prometheus defaults:
# - The Prometheus PVC defaults to an "ssd" StorageClass backed by the GCE PD
#   provisioner, which does not exist on kind — use kind's built-in "standard".
# - kube-scheduler/kube-controller-manager bind to 127.0.0.1 on kind, so their
#   scrape targets can never come up and CL2's health wait would time out —
#   scrape only the apiserver among master components.
# - The apiserver serves on 6443 on kind nodes, not 443.
PROMETHEUS_EXTRA_FLAGS=""
if [ "$PROVIDER" = "kind" ]; then
  PROMETHEUS_PVC_STORAGE_CLASS="${PROMETHEUS_PVC_STORAGE_CLASS:-standard}"
  PROMETHEUS_EXTRA_FLAGS="--prometheus-scrape-apiserver-only=true --prometheus-apiserver-scrape-port=6443"
else
  PROMETHEUS_PVC_STORAGE_CLASS="${PROMETHEUS_PVC_STORAGE_CLASS:-ssd}"
fi

# Paths to local clones (overridable via environment variables).
# Clusterloader2 must be cloned or forked from https://github.com/kubernetes/perf-tests
CL2_DIR="${CL2_DIR:-${HOME}/perf-tests/clusterloader2}"
AGENTS_DIR="${AGENTS_DIR:-${HOME}/agent-sandbox}"
TEST_DIR="${AGENTS_DIR}/dev/load-test/test-recipes"
TEST_CONFIG="${TEST_DIR}/sandbox-capacity-cliff-test.yaml"
LOGS_DIR="${TEST_DIR}/tmp/${RUN_ID}"

# Pre-flight checks
echo "Verifying connection to Kubernetes cluster..."
if ! kubectl cluster-info &> /dev/null; then
  echo "ERROR: Kubernetes cluster is unreachable. Please check your kubeconfig, context, or network connection." >&2
  exit 1
fi

if [ "$KWOK_NODES" = "true" ]; then
  echo "Checking for kwok fake nodes (label type=kwok)..."
  if ! kubectl get nodes -l type=kwok -o name | grep -q .; then
    echo "ERROR: KWOK_NODES=true but no nodes with label type=kwok were found." >&2
    echo "See README-capacity-cliff.md for kwok setup instructions." >&2
    exit 1
  fi
fi

mkdir -p "$LOGS_DIR"

MAX_SANDBOXES=$(($STEP_SIZE * $TOTAL_STEPS * $NAMESPACES))
echo "=== Starting Sandbox Capacity Cliff Test (up to ${MAX_SANDBOXES} sandboxes) ==="
echo "Step Size: $STEP_SIZE, Total Steps: $TOTAL_STEPS, Namespaces: $NAMESPACES, QPS: $QPS"
echo "Hold: $HOLD_DURATION, Convergence Timeout: $CONVERGENCE_TIMEOUT, kwok Nodes: $KWOK_NODES"

# Create overrides specifying the CL2 parameters (jq handles JSON escaping —
# PROMETHEUS_NODE_SELECTOR is a YAML fragment that may contain quotes or
# newlines, which a heredoc would embed as invalid JSON).
jq -n \
  --argjson step_size "$STEP_SIZE" \
  --argjson total_steps "$TOTAL_STEPS" \
  --argjson qps "$QPS" \
  --argjson namespaces "$NAMESPACES" \
  --arg hold_duration "$HOLD_DURATION" \
  --arg convergence_timeout "$CONVERGENCE_TIMEOUT" \
  --arg kwok_nodes "$KWOK_NODES" \
  --arg sandbox_image "$SANDBOX_IMAGE" \
  --arg network_policy "$NETWORK_POLICY" \
  --argjson prometheus_memory_limit_factor "$PROMETHEUS_MEMORY_LIMIT_FACTOR" \
  --arg prometheus_node_selector "$PROMETHEUS_NODE_SELECTOR" \
  --argjson prometheus_slow_apiserver "$PROMETHEUS_SLOW_APISERVER" \
  '{
    CL2_STEP_SIZE: $step_size,
    CL2_TOTAL_STEPS: $total_steps,
    CL2_QPS: $qps,
    CL2_NAMESPACES: $namespaces,
    CL2_HOLD_DURATION: $hold_duration,
    CL2_CONVERGENCE_TIMEOUT: $convergence_timeout,
    CL2_KWOK_NODES: $kwok_nodes,
    CL2_SANDBOX_IMAGE: $sandbox_image,
    CL2_NETWORK_POLICY: $network_policy,
    CL2_PROMETHEUS_MEMORY_LIMIT_FACTOR: $prometheus_memory_limit_factor,
    CL2_PROMETHEUS_NODE_SELECTOR: $prometheus_node_selector,
    CL2_PROMETHEUS_SLOW_APISERVER: $prometheus_slow_apiserver
  }' > "${LOGS_DIR}/testoverrides.json"

# Execute using the cluster loader2 test.
# Exec service is disabled: no measurement in this test execs into pods, and
# its agnhost helper image (registry.k8s.io) is unpullable from private nodes.
cd "$CL2_DIR"
go run cmd/clusterloader.go \
  --enable-exec-service=false \
  --enable-prometheus-server=true \
  --prometheus-pvc-storage-class="${PROMETHEUS_PVC_STORAGE_CLASS}" \
  --prometheus-scrape-kube-proxy="${SCRAPE_KUBE_PROXY}" \
  ${PROMETHEUS_EXTRA_FLAGS} \
  --kubeconfig="${KUBECONFIG}" \
  --prometheus-additional-monitors-path="${TEST_DIR}/monitor" \
  --provider="${PROVIDER}" \
  --report-dir="${LOGS_DIR}" \
  --testconfig="${TEST_CONFIG}" \
  --testoverrides="${LOGS_DIR}/testoverrides.json" \
  --v=2 \
  2>&1 | tee "${LOGS_DIR}/clusterloader2-${RUN_ID}.log"
