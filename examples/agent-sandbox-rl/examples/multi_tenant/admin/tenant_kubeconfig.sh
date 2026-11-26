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

# Write a kubeconfig that acts as one tenant: a short-lived token for the
# ServiceAccount rl-tenant in namespace rl-<user>, each cluster's endpoint and
# CA, and rl-<user> as the default namespace. One context per cluster, named
# rl-<user>@<ctx> after the admin's context name, so that run_experiment.py
# derives the same cluster name for every user; the first is the current
# context. Run as an admin; hand the file to the user (or the pipeline) that
# runs run_experiment.py.
#
#   tenant_kubeconfig.sh <user> [--contexts a,b] [--duration 8h] [-o ~/.kube/rl-<user>.kubeconfig]
set -euo pipefail

user="${1:-}"
if [[ -z "$user" || "$user" == -* ]]; then
  sed -n '/^# Write a kubeconfig/,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 2
fi
shift
contexts=""; duration="8h"; out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --contexts|--context) contexts="$2"; shift 2;;
    --duration) duration="$2"; shift 2;;
    -o|--out) out="$2"; shift 2;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done
out="${out:-$HOME/.kube/rl-$user.kubeconfig}"
ns="rl-$user"

IFS=',' read -r -a ctx_list <<< "$contexts"
if [[ ${#ctx_list[@]} -eq 0 ]]; then ctx_list=("$(kubectl config current-context)"); fi

clusters_yaml=""; users_yaml=""; contexts_yaml=""; first=""
for ctx in "${ctx_list[@]}"; do
  K=(kubectl --context "$ctx")
  cluster="$("${K[@]}" config view -o jsonpath="{.contexts[?(@.name==\"$ctx\")].context.cluster}")"
  server="$("${K[@]}" config view -o jsonpath="{.clusters[?(@.name==\"$cluster\")].cluster.server}")"
  ca="$("${K[@]}" config view --raw -o jsonpath="{.clusters[?(@.name==\"$cluster\")].cluster.certificate-authority-data}")"
  if [[ -z "$ca" ]]; then
    ca_file="$("${K[@]}" config view --raw -o jsonpath="{.clusters[?(@.name==\"$cluster\")].cluster.certificate-authority}")"
    ca="$(base64 < "$ca_file" | tr -d '\n')"
  fi
  token="$("${K[@]}" create token rl-tenant -n "$ns" --duration "$duration")"
  name="rl-$user@$ctx"
  if [[ -z "$first" ]]; then first="$name"; fi
  clusters_yaml+="  - name: $cluster
    cluster:
      server: $server
      certificate-authority-data: $ca
"
  users_yaml+="  - name: $name
    user:
      token: $token
"
  contexts_yaml+="  - name: $name
    context:
      cluster: $cluster
      user: $name
      namespace: $ns
"
done

# umask covers a new file only; an existing one keeps its mode, so tighten it
# before the token lands in it.
umask 077
if [[ -e "$out" ]]; then chmod 600 "$out"; fi
cat > "$out" <<YAML
apiVersion: v1
kind: Config
clusters:
${clusters_yaml}users:
${users_yaml}contexts:
${contexts_yaml}current-context: $first
YAML
names=""; for ctx in "${ctx_list[@]}"; do names+="rl-$user@$ctx "; done
echo "wrote $out (contexts: ${names}current: $first; tokens valid $duration)"
echo "  try:  KUBECONFIG=$out kubectl get sandboxclaims        # own namespace: ok"
echo "        KUBECONFIG=$out kubectl get sandboxclaims -n rl-<other>   # forbidden"
