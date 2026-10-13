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
#
# Runs the recall-memory example on kind and checks, from inside the
# sandboxes:
#
#   1. a sandbox still in the warm pool gets no key;
#   2. a claimed sandbox trades its projected token for a key for
#      agents:<claim name>, and writes memory and working state;
#   3. the claim is deleted and created again; the new pod gets the same
#      namespace, the same memory and the same working state;
#   4. a key for one claim cannot read or resume another claim's agent;
#   5. the sandbox cannot reach polign_db directly.
#
# Needs docker, kind, kubectl, openssl and perl. No account or API key.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-recall-memory}"
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.6}"
KUBECTL="kubectl --context kind-${KIND_CLUSTER_NAME}"

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
since() { perl -e "printf \"%.1f s\", $(now) - $1"; }
step() { printf '\n== %s\n' "$*"; }

# Runs a script inside a pod, after the helpers below. The pod trades its own
# projected token for a key.
in_pod() { { printf '%s\n' "${HELPERS}"; cat; } | ${KUBECTL} -n agents exec -i "$1" -- sh -s; }

# Prints the pod a claim got, once it is Ready.
claim_pod() {
  local pod=""
  for _ in $(seq 1 120); do
    pod="$(${KUBECTL} -n agents get sandboxclaim "$1" -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"
    if [[ -n "${pod}" ]] && ${KUBECTL} -n agents wait --for=condition=Ready "pod/${pod}" --timeout=2s >/dev/null 2>&1; then
      echo "${pod}"
      return
    fi
    sleep 1
  done
  echo "claim $1 never got a Ready pod" >&2
  exit 1
}

claim() {
  sed "s/name: run-1/name: $1/" sandboxclaim.yaml | ${KUBECTL} apply -f - >/dev/null
}

IFS= read -r -d '' HELPERS <<'EOF' || true
exchange() { curl -sS -H "Content-Type: application/json" -d "{\"token\":\"$(cat "$RECALL_TOKEN_FILE")\"}" "$RECALL_URL/v1/auth/kubernetes"; }
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
ANSWER="$(exchange)"
KEY="$(printf '%s' "$ANSWER" | field key)"
AGENT="$(printf '%s' "$ANSWER" | field agent)"
rest() { curl -sS -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d "$2" "$RECALL_URL/v1/$1"; echo; }
# MCP tools/call; prints the tool's result with the JSON escapes removed.
tool() { curl -sS -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}" "$RECALL_URL/mcp" | tr -d '\\'; echo; }
EOF

step "kind cluster ${KIND_CLUSTER_NAME}"
kind get clusters | grep -qx "${KIND_CLUSTER_NAME}" || kind create cluster --name "${KIND_CLUSTER_NAME}" --wait 120s

step "agent-sandbox ${AGENT_SANDBOX_VERSION}"
${KUBECTL} apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml" >/dev/null
${KUBECTL} -n agent-sandbox-system wait --for=condition=Available deployment --all --timeout=180s

step "recall and polign_db"
${KUBECTL} apply -f namespace.yaml -f serviceaccount.yaml
if ! ${KUBECTL} -n recall get secret polign-key >/dev/null 2>&1; then
  ${KUBECTL} -n recall create secret generic polign-key \
    --from-literal=api-key="plgn_$(openssl rand -hex 8)_$(openssl rand -hex 32)"
fi
${KUBECTL} apply -f recall-server.yaml
${KUBECTL} -n recall rollout status deployment/polign-db --timeout=180s
${KUBECTL} -n recall rollout status deployment/recall --timeout=180s

step "template and warm pool"
${KUBECTL} apply -f sandboxtemplate.yaml -f sandboxwarmpool.yaml
for _ in $(seq 1 120); do
  ready="$(${KUBECTL} -n agents get sandboxwarmpool recall-agents -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready:-0}" -ge 2 ]] && break
  sleep 1
done
${KUBECTL} -n agents get sandboxwarmpool recall-agents

step "1. a sandbox still in the warm pool gets no key"
warm="$(${KUBECTL} -n agents get sandbox --no-headers -o custom-columns=NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].kind | awk '$2 == "SandboxWarmPool" {print $1; exit}')"
echo "warm pod: ${warm}"
in_pod "${warm}" <<'EOF'
echo "key: ${KEY:-none}"
echo "$ANSWER" | field message
EOF

step "2. claim run-1, write memory and working state"
t0="$(now)"
claim run-1
pod1="$(claim_pod run-1)"
echo "run-1 is pod ${pod1}, Ready $(since "${t0}") after the claim"
in_pod "${pod1}" <<'EOF'
echo "key for agent $AGENT"
rest remember '{"subject":"user","predicate":"prefers_editor","value":"vim"}' >/dev/null
rest remember '{"subject":"user","predicate":"prefers_editor","value":"helix"}' | grep -o '"value":"[^"]*"' | cut -d'"' -f4 | paste -sd' ' - | awk '{print "remembered " $1 ", superseding " $2}'
tool agent_resume '{"agent_id":"run-1"}' | grep -o '"briefing":"[^"]*' | cut -d'"' -f4 | cut -d. -f1
tool update_working_state '{"agent_id":"run-1","goal":"migrate billing to v2","progress":"call sites listed"}' | grep -o '"goal":"[^"]*"'
tool agent_release '{"agent_id":"run-1"}' | grep -o '"released":[a-z]*'
echo "token exchange, 100 calls (s):"
for i in $(seq 1 100); do curl -sS -o /dev/null -w '%{time_total}\n' -H "Content-Type: application/json" -d "{\"token\":\"$(cat "$RECALL_TOKEN_FILE")\"}" "$RECALL_URL/v1/auth/kubernetes"; done | sort -n | sed -n '50p;99p;100p' | paste -sd' ' - | sed 's/^/  p50 p99 max: /'
echo "recall, 200 calls (s):"
for i in $(seq 1 200); do curl -sS -o /dev/null -w '%{time_total}\n' -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{"subject":"user","predicate":"prefers_editor"}' "$RECALL_URL/v1/recall"; done | sort -n | sed -n '100p;198p;200p' | paste -sd' ' - | sed 's/^/  p50 p99 max: /'
EOF

step "3. delete run-1 and create it again"
${KUBECTL} -n agents delete sandboxclaim run-1 --wait=true
t0="$(now)"
claim run-1
pod1b="$(claim_pod run-1)"
echo "run-1 is now pod ${pod1b} (was ${pod1}), Ready $(since "${t0}") after the claim"
in_pod "${pod1b}" <<'EOF'
echo "key for agent $AGENT"
rest recall '{"subject":"user","predicate":"prefers_editor"}' > /tmp/recall.json
echo "prefers_editor: $(grep -o '"value":"[^"]*"' /tmp/recall.json | tail -1 | cut -d'"' -f4), replaced: $(grep -o '"value":"[^"]*"' /tmp/recall.json | head -1 | cut -d'"' -f4)"
tool agent_resume '{"agent_id":"run-1"}' | grep -o '"goal":"[^"]*"\|"progress":"[^"]*"'
tool agent_release '{"agent_id":"run-1"}' >/dev/null
EOF

step "4. run-2 cannot read or resume run-1"
claim run-2
pod2="$(claim_pod run-2)"
in_pod "${pod2}" <<'EOF'
echo "key for agent $AGENT"
printf 'recall in agents:run-1: '
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{"namespace":"agents:run-1","subject":"user","predicate":"prefers_editor"}' "$RECALL_URL/v1/recall"
printf 'resume run-1: '
tool agent_resume '{"agent_id":"run-1"}' | grep -o 'namespace_mismatch' | head -1
EOF

step "5. the sandbox cannot reach polign_db"
in_pod "${pod2}" <<'EOF'
if curl -s -m 5 -o /dev/null http://polign-db.recall.svc:23000/readyz; then echo "polign_db: reachable"; else echo "polign_db: blocked (timed out after 5 s)"; fi
EOF

step "done"
echo "Delete the cluster with: kind delete cluster --name ${KIND_CLUSTER_NAME}"
