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
#
# Smoke test: apply the (raw, unmodified) manifests in this directory, verify
# no service-account token is mounted, and verify the SSH -> MCP stdio path
# works end to end — both the direct (port-forward) and the sshpiper gateway
# paths. Uses the published agent-box + sshpiper images; kubelet pulls them.
#
# Note on the last check: images/agent-box/entrypoint.sh runs dropbear with
# a *forced command* (`-c /usr/local/bin/agent-box`) — every SSH session
# runs the MCP server regardless of what command the client sends. A plain
# `ssh ... 'echo ok'` smoke test would NOT prove anything (the echo is
# silently discarded); instead this script speaks one round of MCP
# JSON-RPC over the SSH stdio channel and checks for a real response.

set -euo pipefail

# `kubectl wait --for=create` needs kubectl >= 1.31; poll for the pod to exist
# instead so this runs on older clients too. Args: <label-selector> [timeout-s].
wait_for_pod_created() {
  local selector="$1" timeout="${2:-60}" waited=0
  until [ -n "$(kubectl get pod --selector="${selector}" -o name 2>/dev/null)" ]; do
    if [ "${waited}" -ge "${timeout}" ]; then
      echo "timed out after ${timeout}s waiting for a pod matching ${selector}" >&2
      return 1
    fi
    sleep 2
    waited=$((waited + 2))
  done
}

echo "Generating throwaway SSH keypairs (agent + gateway upstream + gateway host)..."
KEYDIR="$(mktemp -d)"
ssh-keygen -t ed25519 -f "${KEYDIR}/agent_ed25519" -N "" -q
ssh-keygen -t ed25519 -f "${KEYDIR}/upstream_ed25519" -N "" -q
ssh-keygen -t ed25519 -f "${KEYDIR}/piper_host_ed25519" -N "" -q

echo "Creating Secrets. The two access paths keep their credentials separate:"
echo "  - direct-box-authorized-keys: the direct/port-forward box trusts the"
echo "    agent key (no gateway in front)."
echo "  - containarium-ssh-key: the gateway boxes trust ONLY the gateway's"
echo "    upstream key — never the agent key directly."
echo "  - agent-authorized-keys: the key the gateway authenticates clients against."
kubectl create secret generic direct-box-authorized-keys \
  --from-file=authorized_keys="${KEYDIR}/agent_ed25519.pub" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic containarium-ssh-key \
  --from-file=authorized_keys="${KEYDIR}/upstream_ed25519.pub" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic agent-authorized-keys \
  --from-file=authorized_keys="${KEYDIR}/agent_ed25519.pub" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Applying the single-box Sandbox (direct/port-forward path)..."
kubectl apply -f containarium-sandbox.yaml

cleanup() {
    echo "Cleaning up..."
    [ -n "${PF_PID:-}" ] && kill "${PF_PID}" 2>/dev/null || true
    kubectl delete --ignore-not-found -f containarium-sandbox.yaml
    kubectl delete --ignore-not-found -f gateway-demo.yaml
    kubectl delete --ignore-not-found -f sshpiper.yaml
    kubectl delete --ignore-not-found secret direct-box-authorized-keys containarium-ssh-key agent-authorized-keys sshpiper-server-key sshpiper-upstream-key
    rm -rf "${KEYDIR}"
}
trap cleanup EXIT

echo "Waiting for sandbox pod to be ready..."
# `kubectl wait --for=condition=ready` errors immediately with "no matching
# resources found" if zero pods match yet — the Sandbox controller needs a
# moment to reconcile the CR into a Pod. Wait for the pod to exist first.
wait_for_pod_created sandbox=containarium-ssh-sandbox 60
kubectl wait --for=condition=ready pod \
  --selector=sandbox=containarium-ssh-sandbox --timeout=180s

echo "Verifying no service-account token is mounted..."
POD_NAME=$(kubectl get pods -l sandbox=containarium-ssh-sandbox \
  -o jsonpath='{.items[0].metadata.name}')
if kubectl exec "${POD_NAME}" -- \
  cat /var/run/secrets/kubernetes.io/serviceaccount/token 2>/dev/null; then
  echo "FAIL: service-account token is mounted in the box" && exit 1
fi
echo "OK: no SA token in the box."

echo "Port-forwarding SSH..."
kubectl port-forward "pod/${POD_NAME}" 2222:2222 &
PF_PID=$!
sleep 5

echo "Checking SSH -> MCP stdio (dropbear's forced command runs agent-box,"
echo "not whatever command the client sends, so we speak MCP directly)..."
INIT_REQUEST='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"kind-smoke-test","version":"0.0.1"}}}'
RESPONSE=$(printf '%s\n' "${INIT_REQUEST}" | timeout 10 ssh -i "${KEYDIR}/agent_ed25519" -p 2222 \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes \
  agent@localhost || true)

if ! printf '%s' "${RESPONSE}" | grep -q '"containarium-agent-box"'; then
  echo "FAIL: no MCP initialize response from agent-box over SSH" >&2
  echo "Got: ${RESPONSE}" >&2
  exit 1
fi
echo "OK: agent-box answered an MCP initialize request over SSH stdio."

echo "=== Gateway path: sshpiper (no kubectl in the agent's data path) ==="
echo "Installing the sshpiper Pipe CRD + gateway..."
kubectl apply -f https://raw.githubusercontent.com/FootprintAI/Containarium/v0.52.1/charts/containarium-k8s/crds/pipe.yaml
kubectl create secret generic sshpiper-server-key \
  --from-file=server_key="${KEYDIR}/piper_host_ed25519" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic sshpiper-upstream-key --type=kubernetes.io/ssh-auth \
  --from-file=ssh-privatekey="${KEYDIR}/upstream_ed25519" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f sshpiper.yaml
kubectl rollout status deployment/sshpiper --timeout=180s

echo "Creating TWO boxes behind the one gateway (box-a, box-b) — the SSH"
echo "username selects which box you land in. Raw manifests, applied as-is:"
kubectl apply -f gateway-demo.yaml
for name in box-a box-b; do
  wait_for_pod_created "sandbox=${name}" 60
  kubectl wait --for=condition=ready pod --selector=sandbox=${name} --timeout=180s
done

echo "Switching boxes by SSH username through the gateway (plain TCP to the"
echo "NodePort — the agent's path holds only its SSH key; kubectl is not"
echo "involved). Each session runs MCP shell_exec(hostname) to prove which"
echo "box answered..."
# Assumes a Linux host: on kind with Docker Desktop (macOS/Windows), the node
# InternalIP is only reachable from inside the Docker VM, so this NodePort
# check won't reach it from the host. Use kind's extraPortMappings there.
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
sleep 5
INITIALIZED_NOTIF='{"jsonrpc":"2.0","method":"notifications/initialized"}'
HOSTNAME_CALL='{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shell_exec","arguments":{"command":"hostname"}}}'
for name in box-a box-b; do
  RESPONSE=$(printf '%s\n%s\n%s\n' "${INIT_REQUEST}" "${INITIALIZED_NOTIF}" "${HOSTNAME_CALL}" \
    | timeout 25 ssh -i "${KEYDIR}/agent_ed25519" -p 32022 \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes \
        "${name}@${NODE_IP}" || true)
  if ! printf '%s' "${RESPONSE}" | grep -q '"containarium-agent-box"'; then
    echo "FAIL: no MCP initialize response from ${name} through the gateway" >&2
    echo "Got: ${RESPONSE}" >&2
    kubectl logs deployment/sshpiper --tail=20 >&2 || true
    exit 1
  fi
  # The pod hostname equals the Sandbox name, so shell_exec(hostname) proves
  # the username routed to the right box — not just to "some" box.
  if ! printf '%s' "${RESPONSE}" | grep -q "${name}"; then
    echo "FAIL: ssh ${name}@gateway answered, but hostname was not ${name}" >&2
    echo "Got: ${RESPONSE}" >&2
    exit 1
  fi
  echo "OK: ssh ${name}@gateway -> MCP shell_exec(hostname) = ${name}"
done
echo "OK: same gateway, same key — the username alone switched the box."

echo "Test finished."
