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

# Warm pods reference the ANTHROPIC_API_KEY secret key (non-optional), so
# without it they never become ready and the wait below would just time out.
# Check the key's contents, not just the Secret's existence: a Secret missing
# ANTHROPIC_API_KEY fails the pod with CreateContainerConfigError all the same.
if [[ -z "$("${KUBECTL[@]}" -n "${NAMESPACE}" get secret claude-code-api-keys \
    -o jsonpath='{.data.ANTHROPIC_API_KEY}' 2>/dev/null || true)" ]]; then
  echo "Secret 'claude-code-api-keys' with key ANTHROPIC_API_KEY not found in namespace '${NAMESPACE}'." >&2
  echo "Create it first (use --from-file so the key stays out of shell history):" >&2
  echo "  ${KUBECTL[*]} -n ${NAMESPACE} create secret generic claude-code-api-keys \\" >&2
  echo "    --from-file=ANTHROPIC_API_KEY=/path/to/anthropic-api-key" >&2
  exit 1
fi

echo "Building claude-code-sandbox:v1..."
docker build -t claude-code-sandbox:v1 .

echo "Loading claude-code-sandbox:v1 into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image claude-code-sandbox:v1 --name "${KIND_CLUSTER_NAME}"

echo "Applying template and warmpool..."
"${KUBECTL[@]}" -n "${NAMESPACE}" apply -f template.yaml -f warmpool.yaml

# Recreate the pool's warm sandboxes on every run: a rebuild under the same
# image tag leaves running pods on the old image, and SandboxTemplate content
# changes do not bump the pool's generation, so neither drift is visible to a
# status-based wait. Deleting by the pool's own selector only touches
# unclaimed warm members (claiming strips the label), never claimed sandboxes.
pool_selector="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool claude-code-pool \
  -o jsonpath='{.status.selector}' 2>/dev/null || true)"
if [[ -n "${pool_selector}" ]]; then
  echo "Recreating warm sandboxes from the current template and image..."
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete sandbox -l "${pool_selector}" --ignore-not-found
fi

echo "Waiting for one warm sandbox to be ready (image pull can take a while)..."
# Poll both fields from a single status read: kubectl <= v1.35 accepts only
# one --for condition, so a two-flag `kubectl wait` would silently drop one.
# observedGeneration alone is not enough either — it does not track template
# rollouts — hence the recreate above plus the readyReplicas check here.
POOL_GENERATION="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/claude-code-pool \
  -o jsonpath='{.metadata.generation}')"
deadline=$(( $(date +%s) + 600 ))
while :; do
  status="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/claude-code-pool \
    -o jsonpath='{.status.readyReplicas}|{.status.observedGeneration}' 2>/dev/null || true)"
  ready="${status%%|*}"
  observed="${status##*|}"
  if [[ "${ready}" == "1" && "${observed}" == "${POOL_GENERATION}" ]]; then
    echo "sandboxwarmpool/claude-code-pool is ready."
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "Timed out after 600s (readyReplicas='${ready}' observedGeneration='${observed}' generation='${POOL_GENERATION}')." >&2
    exit 1
  fi
  sleep 5
done

echo ""
echo "Warm pool ready. Run the demo:"
echo "  SANDBOX_NAMESPACE=${NAMESPACE} SANDBOX_KUBE_CONTEXT=kind-${KIND_CLUSTER_NAME} python3 run_claude.py"
