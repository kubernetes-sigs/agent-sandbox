#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Play the CLUSTER MANAGER half of a hub: create ClusterProfile objects and
# own their identity + health conditions. Nothing here writes capacity --
# that is the fleet-member's job, via demo/publish-clusterprofile.py.
#
# The split matters. Now that the fixture CRD declares `status` as a
# subresource (matching upstream), `kubectl apply -f` DROPS any status block,
# so conditions go through `kubectl patch --subresource=status`. Capacity goes
# through Server-Side Apply from the member, under a different field manager.
# Two writers, two field managers, one status -- which is the whole point of
# stage 2.
#
#   ./demo/seed-clusterprofiles.sh                      # 3 healthy clusters
#   ./demo/seed-clusterprofiles.sh --break-health cluster-c
#
# Env: NS=fleet-system  CTX=  MANAGER=gke-fleet

set -euo pipefail

NS="${NS:-fleet-system}"
CTX="${CTX:-}"
MANAGER="${MANAGER:-gke-fleet}"
# Upstream deprecates v1alpha1 in favour of v1alpha2 while v1alpha1 is still
# the storage version, so both work and v1alpha1 warns. Override to silence it
# once your hub serves v1alpha2. Must match what the publisher writes.
APIVERSION="${APIVERSION:-v1alpha1}"
CLUSTERS="${CLUSTERS:-cluster-a cluster-b cluster-c}"

BREAK_HEALTH=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --break-health) BREAK_HEALTH="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

KUBECTL=(kubectl)
[[ -n "$CTX" ]] && KUBECTL+=(--context "$CTX")

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

"${KUBECTL[@]}" create namespace "$NS" --dry-run=client -o yaml \
  | "${KUBECTL[@]}" apply -f - >/dev/null

# 1. Identity (spec). This is all a cluster manager puts in the object body.
for name in $CLUSTERS; do
  cat <<EOF | "${KUBECTL[@]}" apply -f - >/dev/null
apiVersion: multicluster.x-k8s.io/${APIVERSION}
kind: ClusterProfile
metadata:
  name: ${name}
  namespace: ${NS}
  labels:
    x-k8s.io/cluster-manager: ${MANAGER}
spec:
  displayName: ${name}
  clusterManager:
    name: ${MANAGER}
EOF
done

# 2. Health conditions + version, through the status subresource.
#    Note this deliberately does NOT touch properties: capacity belongs to the
#    member. If this script wrote properties too, the two writers would be
#    fighting over one list and the demo would prove nothing.
for name in $CLUSTERS; do
  healthy="True"
  [[ "$name" == "$BREAK_HEALTH" ]] && healthy="False"
  "${KUBECTL[@]}" -n "$NS" patch clusterprofile "$name" \
    --subresource=status --type=merge -p "$(cat <<EOF
{
  "status": {
    "version": {"kubernetes": "1.31.0"},
    "conditions": [
      {"type": "ControlPlaneHealthy", "status": "${healthy}",
       "reason": "SeededByTestFixture",
       "message": "cluster-manager half of the fixture",
       "lastTransitionTime": "$(now)"},
      {"type": "Joined", "status": "True",
       "reason": "SeededByTestFixture",
       "message": "cluster-manager half of the fixture",
       "lastTransitionTime": "$(now)"}
    ]
  }
}
EOF
)" >/dev/null
done

echo
"${KUBECTL[@]}" -n "$NS" get clusterprofiles
echo
echo "identity + conditions seeded. NO capacity yet -- the clusters will read"
echo "as fresh-but-empty until a member publishes:"
echo "  ./demo/publish-clusterprofile.py"
