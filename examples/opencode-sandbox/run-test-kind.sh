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

set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox}"
NAMESPACE="${SANDBOX_NAMESPACE:-default}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Pin every kubectl call to the cluster kind loads the image into, so a
# different current context cannot split the deploy across two clusters.
KUBECTL=(kubectl --context "kind-${KIND_CLUSTER_NAME}")

cd "${SCRIPT_DIR}"

echo "Building opencode-sandbox:v1..."
docker build -t opencode-sandbox:v1 .

echo "Loading opencode-sandbox:v1 into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image opencode-sandbox:v1 --name "${KIND_CLUSTER_NAME}"

echo "Applying template and warmpool..."
"${KUBECTL[@]}" -n "${NAMESPACE}" apply -f template.yaml -f warmpool.yaml

# Recreate the pool's warm sandboxes on every run: a rebuild under the same
# image tag leaves running pods on the old image, and SandboxTemplate content
# changes do not bump the pool's generation, so neither drift is visible to a
# status-based wait. Deleting by the pool's own selector only touches
# unclaimed warm members (claiming strips the label), never claimed sandboxes.
pool_selector="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool opencode-pool \
  -o jsonpath='{.status.selector}' 2>/dev/null || true)"
if [[ -n "${pool_selector}" ]]; then
  echo "Recreating warm sandboxes from the current template and image..."
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete sandbox -l "${pool_selector}" --ignore-not-found
fi

echo "Waiting for one warm sandbox to be ready (image pull can take a while)..."
# Poll both fields from a single status read: kubectl <= v1.35 accepts only
# one --for condition, so a two-flag `kubectl wait` would silently drop one.
POOL_GENERATION="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/opencode-pool \
  -o jsonpath='{.metadata.generation}')"
deadline=$(( $(date +%s) + 600 ))
while :; do
  status="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/opencode-pool \
    -o jsonpath='{.status.readyReplicas}|{.status.observedGeneration}' 2>/dev/null || true)"
  ready="${status%%|*}"
  observed="${status##*|}"
  if [[ "${ready}" == "1" && "${observed}" == "${POOL_GENERATION}" ]]; then
    echo "sandboxwarmpool/opencode-pool is ready."
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "Timed out after 600s (readyReplicas='${ready}' observedGeneration='${observed}' generation='${POOL_GENERATION}')." >&2
    exit 1
  fi
  sleep 5
done

echo ""
echo "Warm pool ready. Run the SDK demo:"
echo "  SANDBOX_NAMESPACE=${NAMESPACE} SANDBOX_KUBE_CONTEXT=kind-${KIND_CLUSTER_NAME} python3 run_opencode.py"
