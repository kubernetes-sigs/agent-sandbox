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

# Provision one tenant: namespace rl-<user>, RBAC, quota, limits, network policy.
# Run as a cluster admin. Re-running is safe (kubectl apply).
#
#   provision.sh <user> [--context ctx] [--user-subject name]
#                [--pods 16] [--cpu 4] [--memory 8Gi] [--claims 16]
#                [--max-cpu 2] [--max-memory 4Gi] [--controller-namespace agent-sandbox-system]
#                [--shared-namespace rl-shared] [--dry-run] [--delete]
#
# --user-subject binds a User identity next to the tenant's ServiceAccount
# (default: the user name). --delete removes the tenant's namespace and its
# cluster-scoped bindings. --shared-namespace is the namespace the admin
# pre-pulls from (created if missing; "" to skip).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
user="${1:-}"
if [[ -z "$user" || "$user" == -* ]]; then
  sed -n '16,27p' "$0" | sed 's/^# \{0,1\}//'; exit 2
fi
shift

context=""; user_subject="$user"; pods=16; cpu=4; memory=8Gi; claims=16
max_cpu=2; max_memory=4Gi; controller_ns=agent-sandbox-system; shared_ns=rl-shared
dry_run=0; delete=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) context="$2"; shift 2;;
    --user-subject) user_subject="$2"; shift 2;;
    --pods) pods="$2"; shift 2;;
    --cpu) cpu="$2"; shift 2;;
    --memory) memory="$2"; shift 2;;
    --claims) claims="$2"; shift 2;;
    --max-cpu) max_cpu="$2"; shift 2;;
    --max-memory) max_memory="$2"; shift 2;;
    --controller-namespace) controller_ns="$2"; shift 2;;
    --shared-namespace) shared_ns="$2"; shift 2;;
    --dry-run) dry_run=1; shift;;
    --delete) delete=1; shift;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

K=(kubectl)
if [[ -n "$context" ]]; then K+=(--context "$context"); fi

render() {
  python3 "$here/render.py" "$here/tenant.yaml" \
    "TENANT=$user" "TENANT_USER=$user_subject" "CONTROLLER_NAMESPACE=$controller_ns" \
    "QUOTA_PODS=$pods" "QUOTA_CPU=$cpu" "QUOTA_MEMORY=$memory" "QUOTA_CLAIMS=$claims" \
    "LIMIT_MAX_CPU=$max_cpu" "LIMIT_MAX_MEMORY=$max_memory"
}

if [[ "$delete" == "1" ]]; then
  echo "deleting tenant $user (namespace rl-$user and its bindings)"
  "${K[@]}" delete clusterrolebinding "rl-tenant-cluster-read-$user" --ignore-not-found
  "${K[@]}" delete rolebinding "rl-tenant-controller-view-$user" -n "$controller_ns" --ignore-not-found
  "${K[@]}" delete namespace "rl-$user" --ignore-not-found --wait=false
  exit 0
fi

if [[ "$dry_run" == "1" ]]; then
  cat "$here/cluster-read.yaml"; echo "---"; render; exit 0
fi

"${K[@]}" apply -f "$here/cluster-read.yaml"
render | "${K[@]}" apply -f -
if [[ -n "$shared_ns" ]]; then
  "${K[@]}" create namespace "$shared_ns" --dry-run=client -o yaml | "${K[@]}" apply -f -
fi
echo
echo "tenant $user is provisioned in namespace rl-$user"
echo "  kubeconfig for its ServiceAccount:  $here/tenant_kubeconfig.sh $user${context:+ --context $context}"
echo "  quota:                             kubectl describe resourcequota rl-tenant -n rl-$user"
