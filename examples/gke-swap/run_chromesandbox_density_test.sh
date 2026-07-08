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

set -eo pipefail

# Set default image prefix/tag targeting the official staging artifacts, if not provided
IMAGE_PREFIX=${IMAGE_PREFIX:-"us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/"}
IMAGE_TAG=${IMAGE_TAG:-"latest-main"}

# Get the directory of this script
DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
REPO_ROOT="${DIR}/../.."

cd "${REPO_ROOT}"
export KUBECONFIG="${KUBECONFIG:-"${REPO_ROOT}/bin/KUBECONFIG"}"
mkdir -p "$(dirname "${KUBECONFIG}")"
if [ ! -f "${KUBECONFIG}" ] && [ -f ~/.kube/config ]; then
  cp ~/.kube/config "${KUBECONFIG}"
fi

# Determine the densities to test. Default to 120 160 200 240 if not specified.
# Can be overridden by setting DENSITIES env var (e.g. DENSITIES="60 80 100")
IFS=' ' read -r -a DENSITY_LIST <<< "${DENSITIES:-120 160 200 240}"

# Clean up previous artifacts
rm -rf "${DIR}/artifacts"
mkdir -p "${DIR}/artifacts"

# Detect if we are on a GKE cluster with the expected node pools
echo "Detecting cluster nodes..."
GKE_POOLS=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.labels.cloud\.google\.com/gke-nodepool}{"\n"}{end}' 2>/dev/null | sort -u | grep -v '^$' || true)

NODE_SCENARIOS=()
if [ -n "${SCENARIOS}" ]; then
  echo "Using user-specified scenarios: ${SCENARIOS}"
  IFS=' ' read -r -a NODE_SCENARIOS <<< "${SCENARIOS}"
elif [ -n "${GKE_POOLS}" ]; then
  echo "Found GKE node pools:"
  echo "${GKE_POOLS}"
  for pool in ${GKE_POOLS}; do
    if [ "${pool}" != "default-pool" ]; then
      NODE_SCENARIOS+=("${pool}")
    fi
  done
else
  # If not GKE, find all worker nodes
  echo "GKE node pools not detected. Finding available worker nodes..."
  WORKERS=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' || true)
  if [ -z "${WORKERS}" ]; then
    # Fallback to all nodes
    WORKERS=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  fi
  for worker in ${WORKERS}; do
    NODE_SCENARIOS+=("${worker}")
  done
fi

echo "Scenarios to test: ${NODE_SCENARIOS[*]}"
echo "Densities to test: ${DENSITY_LIST[*]}"
echo "--------------------------------------------------"

# Run the matrix of scenarios and densities
for scenario in "${NODE_SCENARIOS[@]}"; do
  # Find a node matching this scenario
  if [ -n "${GKE_POOLS}" ]; then
    NODE_NAME=$(kubectl get nodes -l "cloud.google.com/gke-nodepool=${scenario}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  else
    NODE_NAME="${scenario}"
  fi

  if [ -z "${NODE_NAME}" ]; then
    echo "Warning: No node found for scenario '${scenario}'. Skipping."
    continue
  fi

  for density in "${DENSITY_LIST[@]}"; do
    echo "=================================================="
    echo "Running Scenario: ${scenario} (Node: ${NODE_NAME}), Density: ${density}"
    echo "=================================================="

    # Set framework-level artifacts directory (used by the test context setup)
    export ARTIFACTS="${DIR}/artifacts/${scenario}/${density}"

    # Run the density test via go test using flags
    # We set a generous timeout of 2 hours per run
    if go test ./test/e2e/extensions/ -run ^TestChromeSandboxDensity$ -v -timeout 2h -args \
        -run-perf-load-test \
        -node-name="${NODE_NAME}" \
        -density="${density}" \
        -image-prefix="${IMAGE_PREFIX}" \
        -image-tag="${IMAGE_TAG}"; then
      echo "Scenario ${scenario} at density ${density} completed successfully."
    else
      echo "Scenario ${scenario} at density ${density} failed."
    fi
  done
done

echo "--------------------------------------------------"
echo "All performance tests completed."
echo "Generating summary report..."
echo "--------------------------------------------------"

# Run python helper to compile and print the Markdown table
python3 -c '
import json, glob, os
files = glob.glob("'"${DIR}"'/artifacts/**/density_metrics.json", recursive=True)
if not files:
    print("No results found.")
    exit(0)

artifacts_root = os.path.join("'"${DIR}"'", "artifacts")

print("\n### Performance Density Test Results Summary\n")
print("| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Pod Running (Avg/P99) | Chrome Ready (Avg/P99) | Total Time (Avg/P99) |")
print("|---|---|---|---|---|---|")

# Sort by scenario and then by density numerically
def get_key(filepath):
    rel = os.path.relpath(filepath, artifacts_root)
    parts = rel.split(os.sep)
    try:
        density_val = int(parts[1])
    except (IndexError, ValueError):
        density_val = 0
    scenario = parts[0] if len(parts) > 0 else "unknown"
    return (scenario, density_val)

for f in sorted(files, key=get_key):
    rel = os.path.relpath(f, artifacts_root)
    parts = rel.split(os.sep)
    scenario = parts[0] if len(parts) > 0 else "unknown"
    density = parts[1] if len(parts) > 1 else "unknown"
    try:
        with open(f) as fh:
            data = json.load(fh)
            summary = data.get("summary", {})
            
            def fmt(metric):
                m = summary.get(metric, {})
                avg = m.get("avg", 0)
                p99 = m.get("p99", 0)
                if avg == 0 and p99 == 0:
                    return "N/A"
                return f"{avg:.2f}s / {p99:.2f}s"
                
            sr = fmt("sandbox_ready")
            pr = fmt("pod_running")
            cr = fmt("chrome_ready")
            tot = fmt("total")
            print(f"| {scenario} | {density} | {sr} | {pr} | {cr} | {tot} |")
    except Exception as e:
        print(f"| {scenario} | {density} | Error reading metrics: {e} | | | |")
' | tee "${DIR}/artifacts/perf_test_summary.md"

echo "Summary report saved to ${DIR}/artifacts/perf_test_summary.md"
