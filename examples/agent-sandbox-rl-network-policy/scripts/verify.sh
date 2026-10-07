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

# Checks that a namespace runs on the one-policy-per-namespace layout:
#   1. the namespace-wide policy exists and selects app=agent-sandbox-rl;
#   2. every agent-sandbox-rl SandboxTemplate is Unmanaged;
#   3. no NetworkPolicy owned by an agent-sandbox-rl template is left (the
#      controller deletes them); other teams' templates are not counted;
#   4. (Cilium only, informational) how many identities still carry per-pod keys.
# Usage: NAMESPACE=agent-sandbox-rl [KUBE_CONTEXT=...] scripts/verify.sh
# Exits non-zero if 1 to 3 fail.
set -uo pipefail

NAMESPACE="${NAMESPACE:-agent-sandbox-rl}"
POLICY_NAME="${POLICY_NAME:-agent-sandbox-rl-fleet}"
K=(kubectl)
if [[ -n "${KUBE_CONTEXT:-}" ]]; then K+=(--context "$KUBE_CONTEXT"); fi

pass=0; fail=0
ok()   { echo "PASS  $*"; pass=$((pass+1)); }
bad()  { echo "FAIL  $*"; fail=$((fail+1)); }
info() { echo "INFO  $*"; }

# 1. The namespace-wide policy.
selector="$("${K[@]}" get networkpolicy "$POLICY_NAME" -n "$NAMESPACE" \
  -o jsonpath='{.spec.podSelector.matchLabels.app}' 2>/dev/null || true)"
if [[ "$selector" == "agent-sandbox-rl" ]]; then
  ok "NetworkPolicy $NAMESPACE/$POLICY_NAME selects app=agent-sandbox-rl"
else
  bad "NetworkPolicy $NAMESPACE/$POLICY_NAME missing or not selecting app=agent-sandbox-rl (got '${selector}')"
fi

# 2. Templates created by the SDK are Unmanaged. A failed list is a failure, not
#    an empty fleet: an empty list would skip this check and the next one.
templates_ok=1
if ! templates="$("${K[@]}" get sandboxtemplates.extensions.agents.x-k8s.io -n "$NAMESPACE" \
    -l app=agent-sandbox-rl -o json 2>&1)"; then
  templates_ok=0
  bad "could not list SandboxTemplates in $NAMESPACE: ${templates//$'\n'/ }"
fi
if [[ "$templates_ok" == "1" ]]; then
  total="$(printf '%s' "$templates" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["items"]))')"
  managed="$(printf '%s' "$templates" | python3 -c '
import sys,json
items=json.load(sys.stdin)["items"]
bad=[t["metadata"]["name"] for t in items if (t.get("spec") or {}).get("networkPolicyManagement")!="Unmanaged"]
print(f"{len(bad)} (first: {chr(32).join(bad[:5])})" if bad else "")')"
  if [[ "$total" == "0" ]]; then
    info "no agent-sandbox-rl SandboxTemplates in $NAMESPACE yet (warm a pool, then rerun)"
  elif [[ -z "$managed" ]]; then
    ok "all $total agent-sandbox-rl SandboxTemplates are Unmanaged"
  else
    bad "agent-sandbox-rl SandboxTemplates still Managed: $managed"
  fi
fi

# 3. No policy owned by an agent-sandbox-rl template remains. Policies of other
#    templates in the namespace (other teams, Managed on purpose) are not counted.
if [[ "$templates_ok" == "0" ]]; then
  bad "skipped the policy ownership check: it needs the SandboxTemplate list"
elif ! policies="$("${K[@]}" get networkpolicies -n "$NAMESPACE" -o json 2>&1)"; then
  bad "could not list NetworkPolicies in $NAMESPACE: ${policies//$'\n'/ }"
else
  rl_templates="$(printf '%s' "$templates" | python3 -c '
import sys,json
print(" ".join(t["metadata"]["name"] for t in json.load(sys.stdin)["items"]))')"
  owned="$(printf '%s' "$policies" | RL_TEMPLATES="$rl_templates" python3 -c '
import os,sys,json
rl=set(os.environ.get("RL_TEMPLATES","").split())
names=[]
for np in json.load(sys.stdin)["items"]:
    for o in np["metadata"].get("ownerReferences") or []:
        if o.get("kind")=="SandboxTemplate" and o.get("name") in rl:
            names.append(np["metadata"]["name"])
print(len(names), " ".join(names[:5]))')"
  count="${owned%% *}"
  if [[ "$count" == "0" ]]; then
    ok "no NetworkPolicies owned by agent-sandbox-rl templates in $NAMESPACE"
  else
    bad "$count NetworkPolicies owned by agent-sandbox-rl templates still present (first: ${owned#* }); wait for the controller or check the template mode"
  fi
fi
all_np="$("${K[@]}" get networkpolicies -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
info "NetworkPolicies in $NAMESPACE: $all_np"

# 4. Cilium identities (informational: the label filter is a cluster-side change).
if "${K[@]}" get crd ciliumidentities.cilium.io >/dev/null 2>&1; then
  "${K[@]}" get ciliumidentities -o json 2>/dev/null | python3 -c '
import sys,json
items=json.load(sys.stdin)["items"]
keys=("k8s:agents.x-k8s.io/sandbox-name-hash","k8s:agents.x-k8s.io/claim-uid",
      "k8s:agents.x-k8s.io/warm-pool-sandbox","k8s:agent-sandbox-rl/run-id")
n=len(items); per_pod=sum(1 for i in items if any(k in (i.get("security-labels") or {}) for k in keys))
print(f"INFO  CiliumIdentities: {n} total, {per_pod} carrying a per-pod or per-run sandbox key")
if per_pod:
    print("INFO  those keys are still identity-relevant; see the README section on Cilium")'
else
  info "no CiliumIdentity CRD on this cluster; identity check skipped"
fi

echo "== $pass passed, $fail failed =="
[[ "$fail" -eq 0 ]]
