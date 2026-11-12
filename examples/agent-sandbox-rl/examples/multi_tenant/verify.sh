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

# Show what each tenant's namespace holds and that tenants cannot see each
# other. Run as an admin while run_many.py is live (counts and quota use) or
# after it (everything back to zero). Impersonates each tenant's
# ServiceAccount for the cross-namespace check.
#
#   verify.sh alice bob [--context ctx]
#
# Per namespace: templates, pools, claims, sandboxes and pods of
# agent-sandbox-rl fleets, grouped by run id; ResourceQuota used/hard; the
# NetworkPolicy; and kubelet "Pulled" events, split into pulls from the registry
# and "already present on machine", which is the shared node cache at work.
set -uo pipefail

users=(); context=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) context="$2"; shift 2;;
    -*) echo "unknown flag: $1" >&2; exit 2;;
    *) users+=("$1"); shift;;
  esac
done
if [[ ${#users[@]} -lt 1 ]]; then sed -n '16,27p' "$0" | sed 's/^# \{0,1\}//'; exit 2; fi
K=(kubectl)
if [[ -n "$context" ]]; then K+=(--context "$context"); fi
SEL="app=agent-sandbox-rl"
fail=0

count() { "${K[@]}" get "$1" -n "$2" -l "$SEL" --no-headers 2>/dev/null | wc -l | tr -d ' '; }

# Pods of the fleets, live and terminating apart: a released sandbox's pod is
# gone within its 1 s grace period, but it is still listed for a moment and the
# quota no longer counts it.
pods_summary() {
  "${K[@]}" get pods -n "$1" -l "$SEL" -o json 2>/dev/null | python3 -c '
import json,sys
items=json.load(sys.stdin)["items"]
gone=sum(1 for p in items if p["metadata"].get("deletionTimestamp"))
print(f"{len(items)-gone} (+{gone} terminating)" if gone else str(len(items)))'
}

# Warm pools by run id: pools, and how many report all replicas ready.
pools_by_run() {
  "${K[@]}" get sandboxwarmpools.extensions.agents.x-k8s.io -n "$1" -l "$SEL" -o json 2>/dev/null | python3 -c '
import json,sys
runs={}
for p in json.load(sys.stdin)["items"]:
    rid=p["metadata"].get("labels",{}).get("agents.x-k8s.io/asrl-run-id","?")
    want=(p.get("spec") or {}).get("replicas") or 0
    ready=(p.get("status") or {}).get("readyReplicas") or 0
    n,ok=runs.get(rid,(0,0)); runs[rid]=(n+1, ok+(1 if want and ready>=want else 0))
for rid,(n,ok) in sorted(runs.items()):
    print(f"     run {rid}: {n} pool(s), {ok} ready")'
}

for u in "${users[@]}"; do
  ns="rl-$u"
  echo "== $ns"
  if ! "${K[@]}" get namespace "$ns" >/dev/null 2>&1; then
    echo "   FAIL namespace missing (admin/provision.sh $u)"; fail=1; continue
  fi
  printf '   templates=%s warmpools=%s claims=%s sandboxes=%s pods=%s\n' \
    "$(count sandboxtemplates.extensions.agents.x-k8s.io "$ns")" \
    "$(count sandboxwarmpools.extensions.agents.x-k8s.io "$ns")" \
    "$(count sandboxclaims.extensions.agents.x-k8s.io "$ns")" \
    "$(count sandboxes.agents.x-k8s.io "$ns")" \
    "$(pods_summary "$ns")"
  pools_by_run "$ns"
  "${K[@]}" get resourcequota rl-tenant -n "$ns" -o jsonpath='{range .status.hard}{end}' >/dev/null 2>&1 \
    && "${K[@]}" get resourcequota rl-tenant -n "$ns" -o json 2>/dev/null | python3 -c '
import json,sys
q=json.load(sys.stdin); hard=q["status"].get("hard",{}); used=q["status"].get("used",{})
print("   quota: " + "  ".join("{}={}/{}".format(k, used.get(k, 0), v) for k, v in sorted(hard.items())))' \
    || { echo "   FAIL no ResourceQuota rl-tenant"; fail=1; }
  if "${K[@]}" get networkpolicy agent-sandbox-rl-fleet -n "$ns" >/dev/null 2>&1; then
    echo "   networkpolicy: agent-sandbox-rl-fleet"
  else
    echo "   FAIL no NetworkPolicy agent-sandbox-rl-fleet"; fail=1
  fi
  "${K[@]}" get events -n "$ns" --field-selector reason=Pulled -o json 2>/dev/null | python3 -c '
import json,sys
ev=json.load(sys.stdin)["items"]
cached=sum(e.get("count") or 1 for e in ev if "already present" in (e.get("message") or ""))
pulled=sum(e.get("count") or 1 for e in ev if "already present" not in (e.get("message") or ""))
print(f"   image pulls seen by kubelet: from registry={pulled}  already present on machine={cached}")'
done

# Cross-namespace: each tenant, impersonated, reads its own claims and is
# refused on the next tenant's namespace.
if [[ ${#users[@]} -ge 2 ]]; then
  echo "== isolation"
  for i in "${!users[@]}"; do
    me="${users[$i]}"; other="${users[$(( (i + 1) % ${#users[@]} ))]}"
    sa="system:serviceaccount:rl-$me:rl-tenant"
    if "${K[@]}" --as "$sa" get sandboxclaims.extensions.agents.x-k8s.io -n "rl-$me" >/dev/null 2>&1; then
      echo "   PASS $me reads rl-$me"
    else
      echo "   FAIL $me cannot read rl-$me"; fail=1
    fi
    if "${K[@]}" --as "$sa" get sandboxclaims.extensions.agents.x-k8s.io -n "rl-$other" >/dev/null 2>&1; then
      echo "   FAIL $me can read rl-$other"; fail=1
    else
      echo "   PASS $me is refused on rl-$other"
    fi
  done
fi
exit $fail
