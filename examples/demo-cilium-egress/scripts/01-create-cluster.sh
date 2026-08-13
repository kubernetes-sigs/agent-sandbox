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

# Create a GKE Standard cluster WITHOUT Dataplane V2 so we can install upstream Cilium.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"

if gcloud container clusters describe "$CLUSTER" --zone "$ZONE" --project "$PROJECT" >/dev/null 2>&1; then
  echo "Cluster $CLUSTER already exists in $ZONE; skipping create."
else
  echo "Creating GKE Standard cluster $CLUSTER in $ZONE (no Dataplane V2, no network-policy add-on)..."
  gcloud container clusters create "$CLUSTER" \
    --zone "$ZONE" \
    --project "$PROJECT" \
    --release-channel "$RELEASE_CHANNEL" \
    --enable-ip-alias \
    --num-nodes "$NUM_NODES" \
    --machine-type "$MACHINE_TYPE" \
    --image-type COS_CONTAINERD \
    --no-enable-autoupgrade
fi

gcloud container clusters get-credentials "$CLUSTER" --zone "$ZONE" --project "$PROJECT"

# Guard: raw CiliumNetworkPolicy requires that GKE is NOT managing the dataplane.
if kubectl get ds -n kube-system anetd >/dev/null 2>&1; then
  echo "ERROR: Dataplane V2 (anetd) is present. GKE manages Cilium here and raw" >&2
  echo "       CiliumNetworkPolicy is unavailable. Recreate the cluster WITHOUT" >&2
  echo "       --enable-dataplane-v2 (and without --enable-network-policy)." >&2
  exit 1
fi
# Also reject the legacy GKE network-policy add-on (Calico) — it manages policy too.
if [ "$(gcloud container clusters describe "$CLUSTER" --zone "$ZONE" --project "$PROJECT" \
        --format='value(networkPolicy.enabled)' 2>/dev/null)" = "True" ]; then
  echo "ERROR: the GKE network-policy add-on is enabled. Recreate the cluster" >&2
  echo "       without --enable-network-policy so upstream Cilium can own policy." >&2
  exit 1
fi
echo "OK: cluster ready with a replaceable dataplane; safe to install upstream Cilium."
