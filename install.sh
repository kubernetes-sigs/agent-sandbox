#!/bin/sh
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

# Installs the agent-sandbox controller into a Kubernetes cluster. Rerunning
# upgrades an existing install in place.
#
# Run the copy that matches the release you install, for example:
#   curl -LsSf https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/vX.Y.Z/install.sh | sh -s -- --version vX.Y.Z

set -eu

usage() {
  cat <<'EOF'
Usage: install.sh [--version vX.Y.Z] [--core-only] [--context NAME] [--yes]

  --version     Release to install. Defaults to $AGENT_SANDBOX_VERSION, then latest.
  --core-only   Skip the extensions. Refused if the extension CRDs are installed.
  --context     kubectl context to use. Defaults to the current context.
  --yes         Do not ask for confirmation.
EOF
}

confirm() {
  if (: </dev/tty) 2>/dev/null; then
    printf '%s [y/N] ' "$1" >/dev/tty
    read -r answer </dev/tty
    case $answer in y | Y | yes) return 0 ;; esac
    return 1
  fi
  echo "No terminal to confirm on, pass --yes" >&2
  return 1
}

main() {
  version=${AGENT_SANDBOX_VERSION:-latest}
  manifest=sandbox-with-extensions.yaml
  context=
  yes=
  while [ $# -gt 0 ]; do
    case $1 in
      --version) [ $# -ge 2 ] || { usage >&2; exit 2; }; version=$2; shift ;;
      --core-only) manifest=sandbox.yaml ;;
      --context) [ $# -ge 2 ] || { usage >&2; exit 2; }; context=$2; shift ;;
      --yes) yes=1 ;;
      -h | --help) usage; exit 0 ;;
      *) usage >&2; exit 2 ;;
    esac
    shift
  done

  command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }
  [ -n "$context" ] || context=$(kubectl config current-context)

  if [ "$manifest" = sandbox.yaml ] &&
    kubectl --context "$context" get crd sandboxclaims.extensions.agents.x-k8s.io >/dev/null 2>&1; then
    echo "Extensions are installed in $context, --core-only would stop their controllers" >&2
    exit 1
  fi

  base=https://github.com/kubernetes-sigs/agent-sandbox/releases
  if [ "$version" = latest ]; then
    url=$base/latest/download/$manifest
  else
    url=$base/download/$version/$manifest
  fi

  echo "Installing $manifest ($version) into context $context"
  [ -n "$yes" ] || confirm "Apply CRDs and cluster-wide RBAC to $context?" || exit 1
  kubectl --context "$context" apply -f "$url"
  kubectl --context "$context" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=120s
}

main "$@"
