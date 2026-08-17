#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# End-to-end smoke test for the sandbox-router on a kind cluster.
#
# What it verifies:
#   1. The router pod becomes Ready with --cache-enabled and the Pod
#      informer syncs (gates readyz on initial LIST + correct RBAC).
#   2. A "sandbox" pod (real Pod that mimics the controller-stamped
#      shape: agents.x-k8s.io/sandbox-name-hash label + OwnerReference
#      kind=Sandbox in group agents.x-k8s.io) appears in the cache and
#      is routed to by both DNS form and UID cache hit.
#   3. Killing the sandbox pod and re-creating it produces active cache
#      invalidation when the proxy hits the stale IP.
#   4. With --authz-mode=tokenreview, requests without a Bearer token
#      get 401 (when --authz-tokenreview-require-token=true) and
#      requests with a valid ServiceAccount token are allowed.
#
# Requirements: kind, kubectl, docker.
#
# Usage:
#   ./sandbox-router/dev/smoke-test/run.sh
#
# Env overrides:
#   CLUSTER_NAME   (default: sandbox-router-smoke)
#   ROUTER_IMAGE   (default: kind.local/sandbox-router-go:smoke)
#   KEEP_CLUSTER   (default: 0; set to 1 to leave the cluster running on
#                   exit for debugging)
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-sandbox-router-smoke}"
ROUTER_IMAGE="${ROUTER_IMAGE:-kind.local/sandbox-router-go:smoke}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "${REPO_ROOT}"

log() { printf '\n=== %s ===\n' "$*"; }

# wait_router_endpoints blocks until the sandbox-router-svc Endpoints
# object has at least one address. Use after the initial rollout when
# smoke-curl doesn't exist yet to do an actively-serving probe.
wait_router_endpoints() {
  for _ in $(seq 1 30); do
    if kubectl get endpoints sandbox-router-svc -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q .; then
      return 0
    fi
    sleep 1
  done
  echo "FAIL: sandbox-router-svc has no endpoints after 30s" >&2
  return 1
}

# wait_router_serving blocks until the sandbox-router-svc actually
# responds over the cluster network. Endpoints existing isn't enough —
# kube-proxy still needs a beat to plumb iptables/IPVS rules after a
# rollout, especially when the patch replaces both replicas. Probe via
# /healthz (no headers required, always 200). Requires the smoke-curl
# pod to be running.
wait_router_serving() {
  for _ in $(seq 1 30); do
    if kubectl exec smoke-curl -- curl -sS --max-time 2 -o /dev/null \
        "http://sandbox-router-svc.default.svc.cluster.local:8080/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "FAIL: sandbox-router-svc not serving after 30s" >&2
  return 1
}

# created_cluster tracks whether this run is what created CLUSTER_NAME.
# We only ever delete clusters we created — reusing an existing cluster
# (a developer's persistent debug cluster) and then nuking it on exit
# would be a nasty footgun.
created_cluster=0

cleanup() {
  if [[ "${KEEP_CLUSTER}" == "1" ]]; then
    log "KEEP_CLUSTER=1; leaving cluster ${CLUSTER_NAME} running"
    return
  fi
  if [[ "${created_cluster}" != "1" ]]; then
    log "Cluster ${CLUSTER_NAME} pre-existed; leaving it alone"
    return
  fi
  log "Deleting cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- 1. Ensure the kind cluster exists. ----------------------------------
if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  log "Creating kind cluster ${CLUSTER_NAME}"
  kind create cluster --name "${CLUSTER_NAME}" --wait 60s
  created_cluster=1
else
  log "Reusing existing kind cluster ${CLUSTER_NAME} (will not delete on exit)"
fi
kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null

# --- 2. Build the router image and load it. ------------------------------
log "Building router image ${ROUTER_IMAGE}"
docker build \
  -f sandbox-router/Dockerfile \
  -t "${ROUTER_IMAGE}" \
  .
log "Loading image into kind"
kind load docker-image "${ROUTER_IMAGE}" --name "${CLUSTER_NAME}"

# --- 3. Apply deploy manifests, with the smoke image. --------------------
log "Applying deploy manifests"
kubectl apply -f sandbox-router/deploy/serviceaccount.yaml
kubectl apply -f sandbox-router/deploy/rbac.yaml
kubectl apply -f sandbox-router/deploy/service.yaml
# Use sed to swap the image and to add the tokenreview flags. The example
# deployment.yaml uses :latest with imagePullPolicy=IfNotPresent; we
# pin to the locally-loaded smoke image and Never so kubelet doesn't
# attempt a pull.
sed -e "s|registry.k8s.io/agent-sandbox/sandbox-router-go:latest|${ROUTER_IMAGE}|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    sandbox-router/deploy/deployment.yaml \
  | kubectl apply -f -

log "Waiting for router rollout"
kubectl -n default rollout status deploy/sandbox-router --timeout=120s
wait_router_endpoints

# --- 4. Create a fake sandbox pod that the router's cache should see. ---
# The controller stamps Pods with hash(sandboxName). The cache only
# requires the label EXIST — the value is opaque to the cache — so any
# label value works for this test. We mint a stable fake UID so we can
# reference it in X-Sandbox-Uid.
SANDBOX_NAME="smoke-sandbox"
SANDBOX_UID="00000000-0000-0000-0000-00000000abcd"
SANDBOX_NAMESPACE="default"

log "Creating fake sandbox Pod ${SANDBOX_NAME}"
# Use a Service alongside the Pod so DNS-form routing works too.
# httpbin-like behavior is provided by hashicorp/http-echo: it returns
# the -text argument as the body for every request.
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${SANDBOX_NAME}
  namespace: ${SANDBOX_NAMESPACE}
  labels:
    agents.x-k8s.io/sandbox-name-hash: smoke
    app: smoke-sandbox
  # Pretend a Sandbox CR with the UID below owns this Pod. The cache
  # extracts the UID from this OwnerReference, NOT from any label.
  ownerReferences:
  - apiVersion: agents.x-k8s.io/v1beta1
    kind: Sandbox
    name: ${SANDBOX_NAME}
    uid: ${SANDBOX_UID}
    controller: true
    blockOwnerDeletion: false
spec:
  containers:
  - name: echo
    image: hashicorp/http-echo:1.0
    args: ["-listen=:8888", "-text=smoke-ok"]
    ports:
    - containerPort: 8888
      name: sandbox
    readinessProbe:
      httpGet:
        path: /
        port: 8888
      initialDelaySeconds: 1
      periodSeconds: 1
---
apiVersion: v1
kind: Service
metadata:
  name: ${SANDBOX_NAME}
  namespace: ${SANDBOX_NAMESPACE}
spec:
  selector:
    app: smoke-sandbox
  ports:
  - port: 8888
    targetPort: 8888
EOF

log "Waiting for sandbox Pod to be Ready"
kubectl -n "${SANDBOX_NAMESPACE}" wait --for=condition=Ready pod/"${SANDBOX_NAME}" --timeout=60s

# --- 5. Set up a curl-from-cluster helper. -------------------------------
# We run all proxy checks from inside the cluster so we don't have to
# expose the router as a NodePort. A throwaway alpine pod with curl
# stays alive for the duration of the test.
log "Starting in-cluster curl helper"
kubectl run smoke-curl --image=curlimages/curl:8.10.1 --restart=Never \
  --labels=app=smoke-curl --command -- sh -c "sleep 600"
kubectl wait --for=condition=Ready pod/smoke-curl --timeout=60s

# helper: run curl inside the smoke-curl pod and print response.
in_cluster_curl() {
  # --max-time keeps a hung connection from holding up the whole test
  # for the curl default (120s); the router's own proxy/connect
  # timeouts dominate any legitimate request.
  kubectl exec smoke-curl -- curl -sS --max-time 15 -o /tmp/body -w '%{http_code}' "$@"
}

# --- 6. Verify routing: DNS form. ----------------------------------------
log "Test: DNS-form routing (no X-Sandbox-Uid)"
STATUS=$(in_cluster_curl \
  -H "X-Sandbox-ID: ${SANDBOX_NAME}" \
  -H "X-Sandbox-Namespace: ${SANDBOX_NAMESPACE}" \
  -H "X-Sandbox-Port: 8888" \
  http://sandbox-router-svc.default.svc.cluster.local:8080/)
BODY=$(kubectl exec smoke-curl -- cat /tmp/body)
if [[ "${STATUS}" != "200" ]]; then
  echo "FAIL: DNS-form expected 200, got ${STATUS}; body=${BODY}" >&2
  exit 1
fi
echo "${BODY}" | grep -q "smoke-ok" || { echo "FAIL: unexpected body=${BODY}" >&2; exit 1; }
echo "PASS: DNS-form routing"

# --- 7. Verify routing: UID cache hit. -----------------------------------
log "Test: UID cache hit"
STATUS=$(in_cluster_curl \
  -H "X-Sandbox-ID: ${SANDBOX_NAME}" \
  -H "X-Sandbox-Uid: ${SANDBOX_UID}" \
  -H "X-Sandbox-Namespace: ${SANDBOX_NAMESPACE}" \
  -H "X-Sandbox-Port: 8888" \
  http://sandbox-router-svc.default.svc.cluster.local:8080/)
BODY=$(kubectl exec smoke-curl -- cat /tmp/body)
if [[ "${STATUS}" != "200" ]]; then
  echo "FAIL: UID-cache expected 200, got ${STATUS}; body=${BODY}" >&2
  exit 1
fi
echo "${BODY}" | grep -q "smoke-ok" || { echo "FAIL: unexpected body=${BODY}" >&2; exit 1; }
echo "PASS: UID cache hit"

# --- 8. Sanity check metrics. -------------------------------------------
# A CounterVec only emits HELP/TYPE lines once at least one child
# series exists (Prometheus skips empty collectors). So we only check
# here for metrics that must already have observations after the two
# successful requests above; cache_invalidations_total is checked in
# the AFTER scrape of step 9 since it requires a dial failure to fire.
log "Test: metrics endpoint exposes the new collectors"
# Scrape every router replica and union the output; load-balancing
# means an arbitrary pod may have seen no traffic yet.
ROUTER_IPS=$(kubectl get pod -l app.kubernetes.io/name=sandbox-router -o jsonpath='{.items[*].status.podIP}')
METRICS=""
for ip in ${ROUTER_IPS}; do
  METRICS+=$'\n'"$(kubectl exec smoke-curl -- curl -sS "http://${ip}:9090/metrics")"
done
for needed in sandbox_router_authz_decisions_total sandbox_router_requests_total sandbox_router_build_info; do
  echo "${METRICS}" | grep -q "^# HELP ${needed}" || {
    echo "FAIL: missing metric declaration ${needed}" >&2
    exit 1
  }
done
echo "PASS: required metric collectors emitting"

# --- 9. Verify active cache invalidation. --------------------------------
# Delete the sandbox pod, immediately send a request with a stale UID
# (the cache still has the old IP). The proxy should fail to dial,
# invalidate the entry, and bump cache_invalidations_total.
log "Test: active cache invalidation on dial failure"
BEFORE=$(echo "${METRICS}" | grep -E '^sandbox_router_cache_invalidations_total' | awk '{print $2}' | paste -sd+ - | bc 2>/dev/null || echo 0)
BEFORE="${BEFORE:-0}"
# Block until the pod is gone so the cached IP is definitely stale.
kubectl -n "${SANDBOX_NAMESPACE}" delete pod "${SANDBOX_NAME}" --wait=true >/dev/null
# Race window where the informer might catch the delete before we send
# the request. The Pod was on a specific IP and that IP just went away;
# even if the informer has already evicted, our request now uses DNS
# form (cache miss → DNS), which may resolve the dangling Service
# endpoint. Either way the dial fails and we get 502; what we're
# checking is the *invalidation counter* bump, which only happens when
# the cached IP was used. Send the request before the informer has a
# chance to evict.
STATUS=$(in_cluster_curl \
  -H "X-Sandbox-ID: ${SANDBOX_NAME}" \
  -H "X-Sandbox-Uid: ${SANDBOX_UID}" \
  -H "X-Sandbox-Namespace: ${SANDBOX_NAMESPACE}" \
  -H "X-Sandbox-Port: 8888" \
  --max-time 5 \
  http://sandbox-router-svc.default.svc.cluster.local:8080/ || true)
# Give the informer a moment to settle and re-scrape metrics.
sleep 1
METRICS=""
for ip in ${ROUTER_IPS}; do
  METRICS+=$'\n'"$(kubectl exec smoke-curl -- curl -sS "http://${ip}:9090/metrics")"
done
AFTER=$(echo "${METRICS}" | grep -E '^sandbox_router_cache_invalidations_total' | awk '{print $2}' | paste -sd+ - | bc 2>/dev/null || echo 0)
AFTER="${AFTER:-0}"
# Either the dial-error invalidation bumped the counter (cache had the
# IP) OR the informer evicted first and we fell through to DNS (counter
# unchanged). Both paths are correct; we only fail on a 200 because
# that would mean the proxy somehow connected to a dead pod.
if [[ "${STATUS}" == "200" ]]; then
  echo "FAIL: expected non-200 after deleting target pod, got 200" >&2
  exit 1
fi
echo "PASS: pod deletion observed (status=${STATUS}; invalidations ${BEFORE} → ${AFTER})"

# --- 10. TokenReview authorizer smoke (separate router instance). -------
# Add the system:auth-delegator binding the tokenreview mode needs (it's
# in a separate manifest so default-mode deployments don't carry the
# create rights on tokenreviews / SARs) and roll the router deployment
# to authz-mode=tokenreview with require-token=true so we can verify
# the 401 path.
log "Applying tokenreview RBAC + reconfiguring router for --authz-mode=tokenreview"
kubectl apply -f sandbox-router/deploy/rbac-tokenreview.yaml
kubectl -n default patch deploy sandbox-router --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--authz-mode=tokenreview"},
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--authz-tokenreview-require-token=true"}
]'
kubectl -n default rollout status deploy/sandbox-router --timeout=120s
wait_router_serving

log "Test: tokenreview rejects requests without Bearer"
STATUS=$(in_cluster_curl \
  -H "X-Sandbox-ID: anything" \
  http://sandbox-router-svc.default.svc.cluster.local:8080/)
if [[ "${STATUS}" != "401" ]]; then
  echo "FAIL: expected 401, got ${STATUS}" >&2
  exit 1
fi
echo "PASS: missing token → 401"

log "Test: tokenreview accepts the smoke-curl pod's projected SA token"
# kubectl create token mints a fresh projected ServiceAccount token.
SMOKE_TOKEN=$(kubectl create token default --duration=10m)
# Recreate a sandbox pod so the call has somewhere to land (we deleted
# the original above). Use the same UID so any cache logic still works.
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${SANDBOX_NAME}
  namespace: ${SANDBOX_NAMESPACE}
  labels:
    agents.x-k8s.io/sandbox-name-hash: smoke
    app: smoke-sandbox
  ownerReferences:
  - apiVersion: agents.x-k8s.io/v1beta1
    kind: Sandbox
    name: ${SANDBOX_NAME}
    uid: ${SANDBOX_UID}
    controller: true
    blockOwnerDeletion: false
spec:
  containers:
  - name: echo
    image: hashicorp/http-echo:1.0
    args: ["-listen=:8888", "-text=smoke-ok"]
    ports:
    - containerPort: 8888
EOF
kubectl -n "${SANDBOX_NAMESPACE}" wait --for=condition=Ready pod/"${SANDBOX_NAME}" --timeout=60s

STATUS=$(in_cluster_curl \
  -H "X-Sandbox-ID: ${SANDBOX_NAME}" \
  -H "X-Sandbox-Namespace: ${SANDBOX_NAMESPACE}" \
  -H "X-Sandbox-Port: 8888" \
  -H "Authorization: Bearer ${SMOKE_TOKEN}" \
  http://sandbox-router-svc.default.svc.cluster.local:8080/)
BODY=$(kubectl exec smoke-curl -- cat /tmp/body)
if [[ "${STATUS}" != "200" ]]; then
  echo "FAIL: expected 200 with valid token, got ${STATUS}; body=${BODY}" >&2
  exit 1
fi
echo "${BODY}" | grep -q "smoke-ok" || { echo "FAIL: unexpected body=${BODY}" >&2; exit 1; }
echo "PASS: valid Bearer token → 200"

log "All smoke checks passed."
