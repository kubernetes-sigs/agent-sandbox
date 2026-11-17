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
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Pin every kubectl call to the cluster kind loads the image into, so a
# different current context cannot split the deploy across two clusters.
KUBECTL=(kubectl --context "kind-${KIND_CLUSTER_NAME}")

cd "${SCRIPT_DIR}"

# The runner authenticates to Anthropic with the environment key, so the pod
# cannot start without this Secret. Fail before the slow image build.
if ! "${KUBECTL[@]}" -n "${NAMESPACE}" get secret cma-credentials >/dev/null 2>&1; then
  echo "Secret cma-credentials not found in namespace '${NAMESPACE}'." >&2
  echo "Create it first (see README):" >&2
  echo "  ${KUBECTL[*]} -n ${NAMESPACE} create secret generic cma-credentials \\" >&2
  echo "    --from-file=ANTHROPIC_ENVIRONMENT_ID=/path/to/env-id \\" >&2
  echo "    --from-file=ANTHROPIC_ENVIRONMENT_KEY=/path/to/env-key" >&2
  exit 1
fi

echo "Building cma-runner:v1 (build context: repo root)..."
docker build -f "${SCRIPT_DIR}/Dockerfile" -t cma-runner:v1 "${REPO_ROOT}"

echo "Loading cma-runner:v1 into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image cma-runner:v1 --name "${KIND_CLUSTER_NAME}"

echo "Applying template and warmpool..."
"${KUBECTL[@]}" -n "${NAMESPACE}" apply -f template.yaml -f warmpool.yaml

# Recreate the pool's warm sandboxes on every run: a rebuild under the same
# image tag leaves running pods on the old image, and SandboxTemplate content
# changes do not bump the pool's generation, so neither drift is visible to a
# status-based wait. Deleting by the pool's own selector only touches
# unclaimed warm members (claiming strips the label), never claimed sandboxes.
if ! pool_selector="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool cma-runner-pool \
  -o jsonpath='{.status.selector}')"; then
  echo "Failed to read sandboxwarmpool/cma-runner-pool in namespace '${NAMESPACE}'." >&2
  exit 1
fi
# An empty selector is normal on a first deploy: the controller has not
# populated status yet, and no warm member exists to be stale.
old_uids=""
if [[ -n "${pool_selector}" ]]; then
  # Remember the members being replaced: the pool's readyReplicas can still
  # report the old ones until the controller reconciles, so readiness below
  # must be proven by a member UID that is not in this list. A failed read
  # is fatal: without the old UIDs the wait below would silently accept the
  # stale pool status.
  if ! old_uids="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandbox -l "${pool_selector}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}')"; then
    echo "Failed to list warm sandboxes for sandboxwarmpool/cma-runner-pool." >&2
    exit 1
  fi
  echo "Recreating warm sandboxes from the current template and image..."
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete sandbox -l "${pool_selector}" --ignore-not-found
fi

echo "Waiting for one warm sandbox to be ready..."
# Poll both fields from a single status read: kubectl <= v1.35 accepts only
# one --for condition, so a two-flag `kubectl wait` would silently drop one.
POOL_GENERATION="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/cma-runner-pool \
  -o jsonpath='{.metadata.generation}')"
deadline=$(( $(date +%s) + 600 ))
while :; do
  status="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/cma-runner-pool \
    -o jsonpath='{.status.readyReplicas}|{.status.observedGeneration}' 2>/dev/null || true)"
  ready="${status%%|*}"
  observed="${status##*|}"
  # Pool status alone is never sufficient: it can still report the members
  # this script deleted (or ones deleted before it ran). Success requires a
  # Ready, non-terminating member that is not one of the deleted UIDs.
  fresh_ready=0
  if [[ "${ready}" == "1" && "${observed}" == "${POOL_GENERATION}" ]]; then
    # An empty selector is normal until the controller first populates
    # status; keep waiting. A failed read is fatal: without the selector
    # this check cannot identify pool members.
    if ! sel="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/cma-runner-pool \
      -o jsonpath='{.status.selector}')"; then
      echo "Failed to read status.selector from sandboxwarmpool/cma-runner-pool." >&2
      exit 1
    fi
    if [[ -n "${sel}" ]]; then
      if ! members="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandbox -l "${sel}" \
        -o jsonpath='{range .items[*]}{.metadata.uid} {.status.conditions[?(@.type=="Ready")].status} {.metadata.deletionTimestamp}{"\n"}{end}')"; then
        echo "Failed to list warm sandboxes for the readiness check." >&2
        exit 1
      fi
      while read -r uid is_ready deleting; do
        [[ -z "${uid}" || "${is_ready}" != "True" ]] && continue
        # A terminating member does not prove the replacement is up.
        [[ -n "${deleting}" ]] && continue
        if [[ -z "${old_uids}" ]] || ! grep -qxF "${uid}" <<<"${old_uids}"; then
          fresh_ready=1
        fi
      done <<<"${members}"
    fi
  fi
  if (( fresh_ready )); then
    echo "sandboxwarmpool/cma-runner-pool is ready."
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "Timed out after 600s (readyReplicas='${ready}' observedGeneration='${observed}' generation='${POOL_GENERATION}' fresh_ready='${fresh_ready}')." >&2
    exit 1
  fi
  sleep 5
done

echo ""
echo "Warm pool ready. Next steps:"
echo "  1. export ANTHROPIC_API_KEY=... ANTHROPIC_ENVIRONMENT_ID=..."
echo "  2. cd ${SCRIPT_DIR} && go run ./cmd/controller --namespace ${NAMESPACE} --context kind-${KIND_CLUSTER_NAME}"
echo "  3. go run ./cmd/agent --prompt \"Run 'uname -a' in bash and show the output.\""
