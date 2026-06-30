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

# Install upstream Cilium on the GKE cluster, with the L7 DNS proxy that toFQDNs needs.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"

if ! command -v cilium >/dev/null 2>&1; then
  echo "ERROR: 'cilium' CLI not found on PATH." >&2
  echo "Install it: https://github.com/cilium/cilium-cli/releases (or 'brew install cilium-cli')." >&2
  exit 1
fi

if cilium status >/dev/null 2>&1; then
  echo "Cilium already installed; skipping install."
else
  echo "Installing Cilium (gke.enabled, ipam=kubernetes)..."
  # gke.enabled configures the GKE-specific CNI bin path, node-init and routing.
  # cluster.name must be <=32 chars; GKE's auto-derived name is often too long.
  cluster_name="${CILIUM_CLUSTER_NAME:-agent-sandbox-demo}"
  if [ "${#cluster_name}" -gt 32 ]; then
    echo "ERROR: CILIUM_CLUSTER_NAME '$cluster_name' is ${#cluster_name} chars; Cilium's cluster.name limit is 32." >&2
    exit 1
  fi
  if [ -n "${CILIUM_VERSION:-}" ]; then
    cilium install --version "$CILIUM_VERSION" --set cluster.name="$cluster_name" \
      --set gke.enabled=true --set ipam.mode=kubernetes
  else
    cilium install --set cluster.name="$cluster_name" \
      --set gke.enabled=true --set ipam.mode=kubernetes
  fi
fi

cilium status --wait

# Make sure core DNS is managed by Cilium (so its egress flows through the DNS proxy).
# Fail fast if it doesn't restart — toFQDNs depends on this, and swallowing the
# error would turn the later allow/deny checks into a confusing flake.
kubectl -n kube-system rollout restart deployment kube-dns >/dev/null
kubectl -n kube-system rollout status deployment kube-dns --timeout=180s >/dev/null

echo "OK: Cilium installed and ready."
