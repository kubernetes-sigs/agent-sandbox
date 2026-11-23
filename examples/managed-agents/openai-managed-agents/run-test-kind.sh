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

# Deploys the oma-runner warm pool to a kind cluster. With E2E=true it
# then runs the credential-free link-level test: claim a warm sandbox,
# drive the runner protocol against fakeexec, suspend (pod terminates),
# resume (new pod, workspace PVC reattached), and release.

set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox}"
NAMESPACE="${SANDBOX_NAMESPACE:-default}"
E2E="${E2E:-false}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Pin every kubectl call to the cluster kind loads the image into, so a
# different current context cannot split the deploy across two clusters.
KUBECTL=(kubectl --context "kind-${KIND_CLUSTER_NAME}")

cd "${SCRIPT_DIR}"

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  echo "kind cluster '${KIND_CLUSTER_NAME}' not found." >&2
  echo "Create it first from the repo root: EXTENSIONS=true make deploy-kind" >&2
  exit 1
fi

# The executor authenticates with the restricted environment key, so the pod
# cannot start without this Secret. For the credential-free e2e (fakeexec
# never calls OpenAI) a placeholder is enough; create one if missing. For the
# real quick start, replace it with your own key (see README).
if ! "${KUBECTL[@]}" -n "${NAMESPACE}" get secret oai-env-key >/dev/null 2>&1; then
  echo "WARNING: Secret oai-env-key not found; creating a placeholder." >&2
  echo "WARNING: real codex exec-server runs need a restricted environment key." >&2
  printf 'placeholder-not-a-real-key' | \
    "${KUBECTL[@]}" -n "${NAMESPACE}" create secret generic oai-env-key \
      --from-file=CODEX_API_KEY=/dev/stdin
fi

echo "Building oma-runner:v1 (build context: repo root)..."
docker build -f "${SCRIPT_DIR}/Dockerfile" -t oma-runner:v1 "${REPO_ROOT}"

echo "Loading oma-runner:v1 into kind cluster '${KIND_CLUSTER_NAME}'..."
kind load docker-image oma-runner:v1 --name "${KIND_CLUSTER_NAME}"

echo "Applying template..."
"${KUBECTL[@]}" -n "${NAMESPACE}" apply -f template.yaml

# The e2e patch must land BEFORE the warm pool creates members: patching
# the template underneath a live pool changes the template hash, and the
# controller recycles members mid-wait — a claim can then bind a sandbox
# that is already being replaced.
if [[ "${E2E}" == "true" ]]; then
  echo "E2E mode: pointing the template at fakeexec (no OpenAI calls)..."
  "${KUBECTL[@]}" -n "${NAMESPACE}" patch sandboxtemplate oma-runner-template \
    --type=json -p '[{"op":"replace","path":"/spec/podTemplate/spec/containers/0/env","value":[{"name":"CODEX_API_KEY","valueFrom":{"secretKeyRef":{"name":"oai-env-key","key":"CODEX_API_KEY"}}},{"name":"RUNNER_EXEC_BIN","value":"/usr/local/bin/fakeexec"},{"name":"FAKEEXEC_MODE","value":"ready"}]}]'
fi

echo "Applying warmpool..."
"${KUBECTL[@]}" -n "${NAMESPACE}" apply -f warmpool.yaml

# Recreate the pool's warm sandboxes on every run: a rebuild under the same
# image tag leaves running pods on the old image, and SandboxTemplate content
# changes do not bump the pool's generation, so neither drift is visible to a
# status-based wait. Deleting by the pool's own selector only touches
# unclaimed warm members (claiming strips the label), never claimed sandboxes.
if ! pool_selector="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool oma-runner-pool \
  -o jsonpath='{.status.selector}')"; then
  echo "Failed to read sandboxwarmpool/oma-runner-pool in namespace '${NAMESPACE}'." >&2
  exit 1
fi
# An empty selector is normal on a first deploy: the controller has not
# populated status yet, and no warm member exists to be stale.
old_uids=""
if [[ -n "${pool_selector}" ]]; then
  # Remember the members being replaced: the pool's readyReplicas can still
  # report the old ones until the controller reconciles, so readiness below
  # must be proven by a member UID that is not in this list.
  if ! old_uids="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandbox -l "${pool_selector}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}')"; then
    echo "Failed to list warm sandboxes for sandboxwarmpool/oma-runner-pool." >&2
    exit 1
  fi
  echo "Recreating warm sandboxes from the current template and image..."
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete sandbox -l "${pool_selector}" --ignore-not-found
fi

echo "Waiting for one warm sandbox to be ready..."
POOL_GENERATION="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/oma-runner-pool \
  -o jsonpath='{.metadata.generation}')"
deadline=$(( $(date +%s) + 600 ))
while :; do
  status="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/oma-runner-pool \
    -o jsonpath='{.status.readyReplicas}|{.status.observedGeneration}' 2>/dev/null || true)"
  ready="${status%%|*}"
  observed="${status##*|}"
  # Pool status alone is never sufficient: it can still report the members
  # this script deleted. Success requires a Ready, non-terminating member
  # that is not one of the deleted UIDs.
  fresh_ready=0
  if [[ "${ready}" == "1" && "${observed}" == "${POOL_GENERATION}" ]]; then
    # Reuse the selector read before the loop; re-fetch only on a first
    # deploy, where the controller had not populated status yet.
    sel="${pool_selector}"
    if [[ -z "${sel}" ]]; then
      if ! sel="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxwarmpool/oma-runner-pool \
        -o jsonpath='{.status.selector}')"; then
        echo "Failed to read status.selector from sandboxwarmpool/oma-runner-pool." >&2
        exit 1
      fi
    fi
    if [[ -n "${sel}" ]]; then
      if ! members="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandbox -l "${sel}" \
        -o jsonpath='{range .items[*]}{.metadata.uid} {.status.conditions[?(@.type=="Ready")].status} {.metadata.deletionTimestamp}{"\n"}{end}')"; then
        echo "Failed to list warm sandboxes for the readiness check." >&2
        exit 1
      fi
      while read -r uid is_ready deleting; do
        [[ -z "${uid}" || "${is_ready}" != "True" ]] && continue
        [[ -n "${deleting}" ]] && continue
        if [[ -z "${old_uids}" ]] || ! grep -qxF "${uid}" <<<"${old_uids}"; then
          fresh_ready=1
        fi
      done <<<"${members}"
    fi
  fi
  if (( fresh_ready )); then
    echo "sandboxwarmpool/oma-runner-pool is ready."
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "Timed out after 600s (readyReplicas='${ready}' observedGeneration='${observed}' generation='${POOL_GENERATION}' fresh_ready='${fresh_ready}')." >&2
    exit 1
  fi
  sleep 5
done

if [[ "${E2E}" != "true" ]]; then
  echo ""
  echo "Warm pool ready. Next steps:"
  echo "  1. Create the real credential Secret (see README), then:"
  echo "  2. export OPENAI_API_KEY=..."
  echo "  3. cd ${SCRIPT_DIR} && go run ./cmd/controller --namespace ${NAMESPACE} --context kind-${KIND_CLUSTER_NAME}"
  echo "  4. go run ./cmd/agent --prompt \"Run 'uname -a' in bash and show the output.\""
  exit 0
fi

# ---------------------------------------------------------------------------
# Link-level e2e: claim → runner protocol → suspend → resume → PVC retention
# → release. No OpenAI credentials: the runner supervises fakeexec.
# ---------------------------------------------------------------------------
echo ""
echo "=== Link-level e2e ==="
PF_PID=""
PF_PORT=18080
RESP_DIR="$(mktemp -d)"
RESP_FILE="${RESP_DIR}/exec-response"
cleanup() {
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" 2>/dev/null || true
  rm -rf "${RESP_DIR}"
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete sandboxclaim e2e-claim --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() { echo "E2E FAIL: $*" >&2; exit 1; }

# resource_exists returns 0 while the object still exists OR the lookup
# failed for any reason other than NotFound. Only a confirmed deletion
# returns 1, so a transient apiserver error can never produce a false pass.
resource_exists() { # kind name
  local out
  if out="$("${KUBECTL[@]}" -n "${NAMESPACE}" get "$1" "$2" -o name 2>&1)"; then
    return 0
  fi
  if grep -q "NotFound" <<<"${out}"; then
    return 1
  fi
  echo "WARNING: lookup of $1/$2 failed (treating as still present): ${out}" >&2
  return 0
}

echo "Creating SandboxClaim e2e-claim..."
cat <<'YAML' | "${KUBECTL[@]}" -n "${NAMESPACE}" apply -f -
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: e2e-claim
spec:
  warmPoolRef:
    name: oma-runner-pool
YAML

echo "Waiting for the claim to bind a warm sandbox..."
deadline=$(( $(date +%s) + 120 ))
while :; do
  sbx="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandboxclaim e2e-claim -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"
  [[ -n "${sbx}" ]] && break
  (( $(date +%s) >= deadline )) && fail "claim did not bind within 120s"
  sleep 2
done
echo "Bound sandbox: ${sbx}"

pod_of_sandbox() {
  # The pod name annotation only exists when a sandbox adopted a pod with a
  # divergent name; otherwise the pod carries the sandbox's own name (same
  # fallback as the Go SDK's state resolution).
  local p
  p="$("${KUBECTL[@]}" -n "${NAMESPACE}" get sandbox "$1" \
    -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}' 2>/dev/null || true)"
  [[ -n "${p}" ]] || p="$1"
  echo "${p}"
}
pod="$(pod_of_sandbox "${sbx}")"
pod_uid="$("${KUBECTL[@]}" -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.metadata.uid}')"
[[ -n "${pod_uid}" ]] || fail "sandbox ${sbx} has no pod"
echo "Pod: ${pod} (uid ${pod_uid})"

start_port_forward() {
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
    # Wait for the old forward to release its port before starting a new one.
    wait "${PF_PID}" 2>/dev/null || true
    PF_PID=""
  fi
  # A fresh local port per forward: a health check answered by a stale
  # listener from the previous forward would pass against a dead tunnel.
  PF_PORT=$(( PF_PORT + 1 ))
  "${KUBECTL[@]}" -n "${NAMESPACE}" port-forward "pod/$1" "${PF_PORT}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  deadline=$(( $(date +%s) + 60 ))
  while ! curl -sf --connect-timeout 2 --max-time 5 "http://127.0.0.1:${PF_PORT}/healthz" >/dev/null 2>&1; do
    kill -0 "${PF_PID}" 2>/dev/null || fail "port-forward process exited before becoming reachable"
    (( $(date +%s) >= deadline )) && fail "runner not reachable via port-forward"
    sleep 1
  done
}
start_port_forward "${pod}"

post_exec() { # session_id env_id attempt → HTTP code
  curl -s --connect-timeout 5 --max-time 15 \
    -o "${RESP_FILE}" -w '%{http_code}' -X POST "http://127.0.0.1:${PF_PORT}/executor" \
    -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$1\",\"environment_id\":\"$2\",\"remote_url\":\"wss://codex-cloud-environments.chatgpt.com/e2e\",\"attempt_id\":\"$3\"}"
}
body_has() { grep -q "$1" "${RESP_FILE}"; }

wait_runner_ready() { # timeout-seconds fail-label
  local deadline=$(( $(date +%s) + $1 ))
  while ! curl -s --connect-timeout 2 --max-time 5 "http://127.0.0.1:${PF_PORT}/status" | grep -q '"ready":true'; do
    (( $(date +%s) >= deadline )) && fail "$2: $(curl -s --connect-timeout 2 --max-time 5 "http://127.0.0.1:${PF_PORT}/status")"
    sleep 1
  done
}

echo "POST /executor (fresh)..."
code="$(post_exec e2e-session env-e2e 1)"
[[ "${code}" == "200" ]] && body_has '"started"' || fail "fresh start: HTTP ${code} $(cat "${RESP_FILE}")"

echo "POST /executor (duplicate)..."
code="$(post_exec e2e-session env-e2e 2)"
[[ "${code}" == "200" ]] && body_has 'already_running' || fail "duplicate: HTTP ${code} $(cat "${RESP_FILE}")"

echo "POST /executor (conflicting session)..."
code="$(post_exec other-session env-e2e 1)"
[[ "${code}" == "409" ]] || fail "conflict: HTTP ${code}, want 409"

echo "Checking executor readiness..."
wait_runner_ready 30 "executor never became ready"

echo "Writing probe file to /workspace..."
"${KUBECTL[@]}" -n "${NAMESPACE}" exec "${pod}" -c runner -- sh -c 'echo e2e-probe > /workspace/probe.txt' \
  || fail "could not write probe file"

echo "Stopping executor and suspending the sandbox..."
# max-time must exceed the runner's stop grace (SIGTERM window) or the
# request is cut off before the forced-kill answer arrives.
curl -s --connect-timeout 5 --max-time 60 -X POST "http://127.0.0.1:${PF_PORT}/executor/stop" \
  | grep -q '"stopped"' || fail "executor stop"
"${KUBECTL[@]}" -n "${NAMESPACE}" patch sandbox "${sbx}" --type=merge \
  -p '{"spec":{"operatingMode":"Suspended"}}' >/dev/null
echo "Waiting for the pod to terminate..."
deadline=$(( $(date +%s) + 120 ))
while resource_exists pod "${pod}"; do
  (( $(date +%s) >= deadline )) && fail "pod did not terminate after suspend"
  sleep 2
done
echo "Pod terminated; sandbox object retained: $("${KUBECTL[@]}" -n "${NAMESPACE}" get sandbox "${sbx}" -o name)"

echo "Resuming the sandbox..."
"${KUBECTL[@]}" -n "${NAMESPACE}" patch sandbox "${sbx}" --type=merge \
  -p '{"spec":{"operatingMode":"Running"}}' >/dev/null
echo "Waiting for the replacement pod..."
deadline=$(( $(date +%s) + 300 ))
while :; do
  new_pod="$(pod_of_sandbox "${sbx}" || true)"
  if [[ -n "${new_pod}" ]]; then
    new_uid="$("${KUBECTL[@]}" -n "${NAMESPACE}" get pod "${new_pod}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    # UID-based replacement readiness: a terminating old pod with the same
    # name must not pass as the resumed pod.
    if [[ -n "${new_uid}" && "${new_uid}" != "${pod_uid}" ]]; then
      ready="$("${KUBECTL[@]}" -n "${NAMESPACE}" get pod "${new_pod}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
      [[ "${ready}" == "True" ]] && break
    fi
  fi
  (( $(date +%s) >= deadline )) && fail "replacement pod not ready within 300s"
  sleep 3
done
pod="${new_pod}"
pod_uid="${new_uid}"
echo "Replacement pod ready: ${pod} (uid ${pod_uid})"

start_port_forward "${pod}"

echo "Reconnecting the executor with the original binding..."
code="$(post_exec e2e-session env-e2e 3)"
[[ "${code}" == "200" ]] || fail "reconnect: HTTP ${code} $(cat "${RESP_FILE}")"
wait_runner_ready 300 "executor not ready after resume"

echo "Verifying the workspace PVC survived suspend/resume..."
"${KUBECTL[@]}" -n "${NAMESPACE}" exec "${pod}" -c runner -- grep -q e2e-probe /workspace/probe.txt \
  || fail "probe file lost across suspend/resume"
echo "PVC retention verified."

echo "Releasing the claim..."
[[ -n "${PF_PID}" ]] && kill "${PF_PID}" 2>/dev/null || true; PF_PID=""
"${KUBECTL[@]}" -n "${NAMESPACE}" delete sandboxclaim e2e-claim --wait=false >/dev/null
deadline=$(( $(date +%s) + 180 ))
while resource_exists sandbox "${sbx}"; do
  (( $(date +%s) >= deadline )) && fail "sandbox not deleted after claim release"
  sleep 3
done
echo "Sandbox released."

echo ""
echo "E2E PASS"
