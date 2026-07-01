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
  version=""
  # Prefer gh, but it fails if installed-yet-unauthenticated; tolerate that and
  # fall back to the redirect-based discovery rather than aborting under set -e.
  if command -v gh >/dev/null 2>&1; then
    version="$(gh release view --repo kubernetes-sigs/agent-sandbox --json tagName -q .tagName 2>/dev/null || true)"
  fi
  if [ -z "$version" ]; then
    # Resolve the /releases/latest redirect to its tag.
    version="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
      https://github.com/kubernetes-sigs/agent-sandbox/releases/latest | sed 's#.*/tag/##')"
  fi
fi
echo "Installing agent-sandbox $version ..."

base="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${version}"
kubectl apply --server-side -f "${base}/manifest.yaml"
kubectl apply --server-side -f "${base}/extensions.yaml"

echo "Waiting for the controller to become Available..."
kubectl wait --for=condition=Available deploy --all -n agent-sandbox-system --timeout=180s
echo "OK: agent-sandbox $version installed."
