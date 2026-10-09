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

set -euo pipefail

if [[ "$#" -ne 1 || "$1" == -* ]]; then
  echo "Usage: bash verify.sh <kubectl-context>" >&2
  exit 2
fi

CONTEXT="$1"
NAMESPACE="claimed-sandbox-service"
EXAMPLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KUBECTL=(kubectl "--context=$CONTEXT" "--namespace=$NAMESPACE")
NAMESPACE_UID=""

cleanup() {
  local status="$?"
  trap - EXIT
  if [[ -n "$NAMESPACE_UID" ]]; then
    local current_uid
    if ! current_uid="$("${KUBECTL[@]}" get namespace "$NAMESPACE" --ignore-not-found -o jsonpath='{.metadata.uid}')"; then
      echo "Cannot confirm namespace ownership; leaving it for manual cleanup." >&2
      exit 1
    fi
    if [[ "$current_uid" == "$NAMESPACE_UID" ]]; then
      "${KUBECTL[@]}" delete namespace "$NAMESPACE" --wait=false || status=1
    elif [[ -n "$current_uid" ]]; then
      echo "Namespace UID changed; refusing to delete a replacement namespace." >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

ready_pods() {
  "${KUBECTL[@]}" get endpointslices \
    -l kubernetes.io/service-name=claimed-sandbox-entry \
    -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.targetRef.name}{"\n"}{end}' \
    | sort -u
}

wait_for_ready_count() {
  local expected="$1"
  local deadline=$((SECONDS + 180))
  local actual
  while true; do
    actual="$(ready_pods | awk 'NF { count++ } END { print count+0 }')"
    if [[ "$actual" -eq "$expected" ]]; then
      return
    fi
    if (( SECONDS >= deadline )); then
      echo "Expected $expected eligible endpoints, found $actual." >&2
      ready_pods >&2
      return 1
    fi
    sleep 2
  done
}

# Do not take over or clean up resources from a separate run.
existing="$("${KUBECTL[@]}" get namespace "$NAMESPACE" --ignore-not-found -o name)"
if [[ -n "$existing" ]]; then
  echo "Namespace $NAMESPACE already exists in $CONTEXT; refusing to reuse it." >&2
  exit 1
fi
"${KUBECTL[@]}" get crd sandboxclaims.extensions.agents.x-k8s.io \
  sandboxtemplates.extensions.agents.x-k8s.io \
  sandboxwarmpools.extensions.agents.x-k8s.io >/dev/null
NAMESPACE_UID="$("${KUBECTL[@]}" create -f "$EXAMPLE_DIR/namespace.yaml" -o jsonpath='{.metadata.uid}')"
"${KUBECTL[@]}" apply -k "$EXAMPLE_DIR"
"${KUBECTL[@]}" wait --for=jsonpath='{.status.readyReplicas}'=2 \
  sandboxwarmpool/serving-pool --timeout=180s
wait_for_ready_count 0
echo "PASS: ready, unclaimed warm-pool Pods are not Service backends."

"${KUBECTL[@]}" apply -f "$EXAMPLE_DIR/claims.yaml"
"${KUBECTL[@]}" wait --for=condition=Ready sandboxclaim/serving-a \
  sandboxclaim/serving-b --timeout=180s
"${KUBECTL[@]}" wait --for=condition=Ready pod/allowed-client \
  pod/denied-client --timeout=180s
wait_for_ready_count 2

pod_for_claim() {
  local uid
  uid="$("${KUBECTL[@]}" get sandboxclaim "$1" -o jsonpath='{.metadata.uid}')"
  "${KUBECTL[@]}" get pods -l "agents.x-k8s.io/claim-uid=$uid" \
    -o jsonpath='{.items[0].metadata.name}'
}

POD_A="$(pod_for_claim serving-a)"
POD_B="$(pod_for_claim serving-b)"
if [[ -z "$POD_A" || -z "$POD_B" || "$POD_A" == "$POD_B" ]]; then
  echo "Expected two distinct claimed Pods." >&2
  exit 1
fi
expected_pods="$(printf '%s\n%s\n' "$POD_A" "$POD_B" | sort)"
if [[ "$(ready_pods)" != "$expected_pods" ]]; then
  echo "Service endpoints do not match the two Claims." >&2
  exit 1
fi

# Each curl uses a new connection; distribution is not strict per-request round robin.
responses=""
for ((i = 0; i < 30; i++)); do
  response="$("${KUBECTL[@]}" exec allowed-client -- curl -fsS \
    --connect-timeout 2 --max-time 5 --http1.1 -H 'Connection: close' \
    http://claimed-sandbox-entry/hostname)"
  responses+="$response"$'\n'
done
if [[ "$(printf '%s' "$responses" | sort -u)" != "$expected_pods" ]]; then
  echo "Fresh Service connections did not reach both claimed Pods." >&2
  exit 1
fi
"${KUBECTL[@]}" wait --for=jsonpath='{.status.readyReplicas}'=2 \
  sandboxwarmpool/serving-pool --timeout=180s
echo "PASS: the shared Service reaches both claimed Pods and excludes the warm reserve."

"${KUBECTL[@]}" exec "$POD_A" -c app -- touch /tmp/not-ready
"${KUBECTL[@]}" wait --for=condition=Ready=false "pod/$POD_A" --timeout=60s
wait_for_ready_count 1
if [[ "$(ready_pods)" != "$POD_B" ]]; then
  echo "The unready claimed Pod is still an eligible endpoint." >&2
  exit 1
fi
for ((i = 0; i < 5; i++)); do
  response="$("${KUBECTL[@]}" exec allowed-client -- curl -fsS \
    --connect-timeout 2 --max-time 5 -H 'Connection: close' \
    http://claimed-sandbox-entry/hostname)"
  if [[ "$response" != "$POD_B" ]]; then
    echo "Service did not route exclusively to the remaining ready Pod." >&2
    exit 1
  fi
done
"${KUBECTL[@]}" exec "$POD_A" -c app -- rm /tmp/not-ready
"${KUBECTL[@]}" wait --for=condition=Ready "pod/$POD_A" --timeout=60s
wait_for_ready_count 2
echo "PASS: readiness removes and restores a claimed Pod without replacing it."

# Compare permitted and denied traffic to the same IP, avoiding DNS-failure false positives.
service_ip="$("${KUBECTL[@]}" get service claimed-sandbox-entry -o jsonpath='{.spec.clusterIP}')"
pod_ip="$("${KUBECTL[@]}" get pod "$POD_B" -o jsonpath='{.status.podIP}')"
[[ "$service_ip" != *:* ]] || service_ip="[$service_ip]"
[[ "$pod_ip" != *:* ]] || pod_ip="[$pod_ip]"
for target in "$service_ip:80" "$pod_ip:8000"; do
  "${KUBECTL[@]}" exec allowed-client -- curl -fsS \
    --connect-timeout 2 --max-time 5 "http://$target/hostname" >/dev/null
  if "${KUBECTL[@]}" exec denied-client -- curl -fsS \
    --connect-timeout 2 --max-time 5 "http://$target/hostname" >/dev/null; then
    echo "Denied caller reached $target; NetworkPolicy is not isolating the application." >&2
    exit 1
  else
    status="$?"
    if [[ "$status" -ne 28 ]]; then
      echo "Expected a denied connection timeout, not curl/kubectl exit $status." >&2
      exit 1
    fi
  fi
done
echo "PASS: ingress permits the intended caller and blocks the denied caller."

"${KUBECTL[@]}" delete sandboxclaim serving-a --wait=false
"${KUBECTL[@]}" wait --for=delete "pod/$POD_A" --timeout=180s
wait_for_ready_count 1
if [[ "$(ready_pods)" != "$POD_B" ]]; then
  echo "Claim deletion did not remove the claimed Pod from the Service." >&2
  exit 1
fi
"${KUBECTL[@]}" wait --for=jsonpath='{.status.readyReplicas}'=2 \
  sandboxwarmpool/serving-pool --timeout=180s
response="$("${KUBECTL[@]}" exec allowed-client -- curl -fsS \
  --connect-timeout 2 --max-time 5 http://claimed-sandbox-entry/hostname)"
if [[ "$response" != "$POD_B" ]]; then
  echo "The remaining claimed Pod is not serving after Claim deletion." >&2
  exit 1
fi
echo "PASS: Claim deletion leaves the remaining backend serving; the warm reserve stays excluded."
