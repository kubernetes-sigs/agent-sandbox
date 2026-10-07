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

# Install the agent-sandbox controller (core + extensions) from a published release.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"

version="$AGENT_SANDBOX_VERSION"
if [ "$version" = "latest" ]; then
  # Resolve the /releases/latest redirect to its tag.
  version="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
    https://github.com/kubernetes-sigs/agent-sandbox/releases/latest | sed 's#.*/tag/##')"
fi
echo "Installing agent-sandbox $version ..."

kubectl apply --server-side -f \
  "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${version}/sandbox-with-extensions.yaml"

echo "Waiting for the controllers to become Available..."
kubectl wait --for=condition=Available deploy --all -n agent-sandbox-system --timeout=180s
echo "OK: agent-sandbox $version installed."
