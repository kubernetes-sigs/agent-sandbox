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

# Smoke test: the oracle agent on the Terminal-Bench 2.0 sample, on the current
# kubeconfig context. Every trial must score 1.0.
#
#   examples/harbor-terminal-bench/scripts/run-sample.sh [extra harbor run flags]
#
# Env: NAMESPACE (default harbor), KUBE_CONTEXT (default: current context).
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
NAMESPACE=${NAMESPACE:-harbor}
ctx_flag=()
if [[ -n "${KUBE_CONTEXT:-}" ]]; then ctx_flag=(--context "$KUBE_CONTEXT"); fi

command -v harbor >/dev/null || { echo "harbor is not on PATH: pip install 'harbor>=0.25'"; exit 1; }
python -c "import k8s_agent_sandbox_harbor" 2>/dev/null || {
  echo "k8s_agent_sandbox_harbor is not installed in this Python:"
  echo "  pip install -e clients/python/agentic-sandbox-client[async] -e clients/integrations/harbor"
  exit 1
}
kubectl "${ctx_flag[@]}" get crd sandboxclaims.extensions.agents.x-k8s.io >/dev/null 2>&1 || {
  echo "the cluster does not serve the agent-sandbox extension CRDs (EXTENSIONS=true make deploy-kind)"; exit 1; }

kubectl "${ctx_flag[@]}" apply -f "$here/deploy/namespace.yaml"
if [[ "$NAMESPACE" != "harbor" ]]; then
  kubectl "${ctx_flag[@]}" get namespace "$NAMESPACE" >/dev/null 2>&1 || kubectl "${ctx_flag[@]}" create namespace "$NAMESPACE"
fi

extra=()
[[ -n "${KUBE_CONTEXT:-}" ]] && extra+=(--ek "kube_context=$KUBE_CONTEXT")
echo "== harbor run: terminal-bench-sample@2.0, oracle agent, namespace $NAMESPACE"
harbor run -c "$here/configs/sample.yaml" --ek "namespace=$NAMESPACE" "${extra[@]}" "$@"

echo "== leftovers in $NAMESPACE (expect none):"
kubectl "${ctx_flag[@]}" -n "$NAMESPACE" get sandboxclaims 2>/dev/null || true
echo "== report:"
python "$here/scripts/report.py" jobs/harbor-terminal-bench
