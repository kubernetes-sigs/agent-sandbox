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

# E2E verification of the codelab. Applies the ClusterNetworkPolicies one phase
# at a time, in README order, and asserts the expected allow/deny outcome after
# each. Re-runnable: it first resets the cluster to the phase-1 state.
# Exits non-zero if any assertion fails.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"
M="$(dirname "${BASH_SOURCE[0]}")/../manifests"

pass=0; fail=0

# --- helpers -----------------------------------------------------------------

# Pod name backing the claim named "agent" in a namespace (pod name == sandbox name).
pod_of() { kubectl get sandboxclaim agent -n "$1" -o jsonpath='{.status.sandbox.name}'; }

# HTTP(S) GET from inside a sandbox. Prints "ok" or "fail". Denied traffic is
# dropped, not rejected, so the 5s timeout is what turns a deny into "fail".
# An HTTP error status still means the connection was allowed.
http_probe() { # ns pod url
  kubectl exec -n "$1" "$2" -- python3 -c '
import sys, urllib.request, urllib.error
try:
    urllib.request.urlopen(sys.argv[1], timeout=5).read(1)
    print("ok")
except urllib.error.HTTPError:
    print("ok")
except Exception as e:
    print("fail")' "$3" 2>/dev/null | tail -n1
}

# Plain TCP connect to ip:port, no DNS involved.
tcp_probe() { # ns pod ip port
  kubectl exec -n "$1" "$2" -- python3 -c '
import sys, socket
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5).close()
    print("ok")
except Exception:
    print("fail")' "$3" "$4" 2>/dev/null | tail -n1
}

# Name resolution only.
dns_probe() { # ns pod name
  kubectl exec -n "$1" "$2" -- python3 -c '
import sys, socket
try:
    socket.getaddrinfo(sys.argv[1], 443)
    print("ok")
except Exception:
    print("fail")' "$3" 2>/dev/null | tail -n1
}

# pip needs pypi.org (index) and files.pythonhosted.org (download).
pip_probe() { # ns pod
  kubectl exec -n "$1" "$2" -- sh -c \
    'rm -rf /tmp/pkgs; pip download --no-deps --no-cache-dir --timeout 5 --retries 0 -q -d /tmp/pkgs requests >/dev/null 2>&1 && echo ok || echo fail' \
    2>/dev/null | tail -n1
}

check() { # desc expected(ok|fail) probe args...
  local desc="$1" expected="$2"; shift 2
  local actual
  # An "ok" outcome depends on the public internet and on the DNS answer being
  # seen by the agent (its capture queue is fail-open), so allow a couple of
  # retries. A "fail" outcome is asserted on the first attempt only: a single
  # successful connection means the policy did not deny it.
  for _ in 1 2 3; do
    actual="$("$@")"
    if [ "$expected" = "fail" ] || [ "$actual" = "ok" ]; then break; fi
    sleep 2
  done
  case "$actual" in
    ok|fail) ;;
    *) echo "FAIL: $desc (probe did not run: '$actual')"; fail=$((fail+1)); return;;
  esac
  if [ "$actual" = "$expected" ]; then
    echo "PASS: $desc (=$actual)"; pass=$((pass+1))
  else
    echo "FAIL: $desc (expected $expected, got $actual)"; fail=$((fail+1))
  fi
}

apply() { # manifest
  # Without -e a failed apply would let the phase run against the previous policy set.
  kubectl apply -f "$M/$1" || { echo "FAIL: could not apply $1, aborting"; exit 1; }
  # kube-network-policies exposes no per-policy sync signal, so give its informers
  # a fixed settle time. Propagation on kind is well under a second.
  sleep 5
}

# --- reset to phase-1 state ---------------------------------------------------
echo "== Reset: removing this example's ClusterNetworkPolicies and the raw sandbox from a previous run =="
for f in "${CNP_MANIFESTS[@]}"; do
  kubectl delete -f "$M/$f.yaml" --ignore-not-found >/dev/null
done
kubectl delete -f "$M/90-raw-sandbox.yaml" --ignore-not-found >/dev/null
sleep 3

A="$(pod_of "$NS_A")"; B="$(pod_of "$NS_B")"
TOOLS_IP="$(kubectl get svc tool-server -n "$NS_TOOLS" -o jsonpath='{.spec.clusterIP}')"
TOOLS_URL="http://${TOOLS_IP}:8080/hostname"
DNS_IP="$(kubectl get svc kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}')"
if [ -z "$A" ] || [ -z "$B" ] || [ -z "$TOOLS_IP" ] || [ -z "$DNS_IP" ]; then
  echo "FAIL: demo not deployed (run scripts/04-deploy-demo.sh first)"; exit 1
fi
echo "team-a pod: $A   team-b pod: $B   tool-server: $TOOLS_URL"
echo

echo "== Phase 1: only the template-managed NetworkPolicy =="
check "team-a reaches github.com (any public site is allowed)"        ok   http_probe "$NS_A" "$A" https://github.com
check "team-a reaches example.com (NetworkPolicy cannot say 'only github')" ok http_probe "$NS_A" "$A" https://example.com
check "team-a is denied the in-cluster tool server (RFC1918 carve-out)" fail http_probe "$NS_A" "$A" "$TOOLS_URL"
check "team-b reaches the tool server (its template allows it)"         ok   http_probe "$NS_B" "$B" "$TOOLS_URL"
echo

echo "== Phase 2: Admin-tier default deny (priority 100) =="
apply 30-cnp-admin-default-deny.yaml
check "team-a can no longer resolve names"                              fail dns_probe  "$NS_A" "$A" github.com
check "team-a is denied github.com"                                     fail http_probe "$NS_A" "$A" https://github.com
check "team-b is denied the tool server despite its NetworkPolicy"      fail http_probe "$NS_B" "$B" "$TOOLS_URL"
echo

echo "== Phase 3: allow DNS (priority 10) =="
apply 40-cnp-admin-allow-dns.yaml
check "team-a resolves via the public resolvers"                        ok   dns_probe  "$NS_A" "$A" github.com
check "team-b resolves via CoreDNS (Pass -> its NetworkPolicy allows)"  ok   dns_probe  "$NS_B" "$B" github.com
check "team-a is still denied CoreDNS (Pass -> secure default blocks it)" fail tcp_probe "$NS_A" "$A" "$DNS_IP" 53
check "team-a still cannot connect to github.com"                       fail http_probe "$NS_A" "$A" https://github.com
echo

echo "== Phase 4: FQDN allowlist for GitHub (priority 20) =="
apply 50-cnp-admin-allow-github.yaml
check "team-a reaches github.com"                                       ok   http_probe "$NS_A" "$A" https://github.com
check "team-a reaches api.github.com (wildcard *.github.com)"           ok   http_probe "$NS_A" "$A" https://api.github.com
check "team-b reaches github.com"                                       ok   http_probe "$NS_B" "$B" https://github.com
check "team-a is denied example.com"                                    fail http_probe "$NS_A" "$A" https://example.com
check "team-a is denied a raw IP never returned for an allowed name"    fail tcp_probe  "$NS_A" "$A" 1.1.1.1 443
echo

echo "== Phase 5: per-tenant PyPI allowlist for team-b (priority 30) =="
apply 60-cnp-admin-team-b-allow-pypi.yaml
check "team-b can pip download"                                         ok   pip_probe "$NS_B" "$B"
check "team-a cannot pip download"                                      fail pip_probe "$NS_A" "$A"
echo

echo "== Phase 6: Pass to the NetworkPolicy tier for shared-tools (priority 5) =="
apply 70-cnp-admin-pass-shared-tools.yaml
check "team-b reaches the tool server (Pass -> its NetworkPolicy allows)"   ok   http_probe "$NS_B" "$B" "$TOOLS_URL"
check "team-a is denied the tool server (Pass -> its NetworkPolicy denies)" fail http_probe "$NS_A" "$A" "$TOOLS_URL"
echo

echo "== Phase 7: Baseline tier for sandboxes without a NetworkPolicy =="
kubectl apply -f "$M/90-raw-sandbox.yaml" >/dev/null
kubectl wait --for=condition=Ready sandbox/raw-sandbox -n "$NS_A" --timeout=180s >/dev/null
sleep 3
check "raw sandbox reaches the tool server (Pass -> no NetworkPolicy -> default allow)" ok http_probe "$NS_A" raw-sandbox "$TOOLS_URL"
apply 80-cnp-baseline-default-deny.yaml
check "raw sandbox is denied the tool server (Baseline deny)"          fail http_probe "$NS_A" raw-sandbox "$TOOLS_URL"
check "team-b still reaches the tool server (its NetworkPolicy wins over Baseline)" ok http_probe "$NS_B" "$B" "$TOOLS_URL"
echo

echo "== Summary: $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
