#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Print a snapshot of the fleet across all clusters + GCS state.
# Supports --fail-modes to inject the canned failures documented in
# ARCHITECTURE.md.
#
# Env vars:
#   CLUSTERS  space-separated cluster names (default: "fleet-a fleet-b").
#             NOTE: bash doesn't export arrays; if you have `CLUSTERS=(...)`
#             in your shell, pass it as `CLUSTERS="${CLUSTERS[*]}"` here.
#   NS        namespace (default: multi-cluster-fleet)
#   PROJECT   GKE project. When set, the script runs `gcloud container
#             clusters get-credentials` per cluster to switch current-context
#             (GKE mode). Unset = kind mode (uses --context kind-<name>).
#   REGION    GKE region (required when PROJECT is set)
#   FLEET_BUCKET  GCS bucket for the fleet hub (used in --fail-modes + summary)
#
# Examples:
#   ./demo/observe.sh                                      # kind 2-cluster demo
#   CLUSTERS="cluster-a cluster-b cluster-c" \
#     PROJECT=my-proj REGION=us-central1 ./demo/observe.sh # live GKE fleet
set -euo pipefail

CLUSTERS_STR="${CLUSTERS:-fleet-a fleet-b}"
read -ra CLUSTERS <<< "$CLUSTERS_STR"
NS="${NS:-multi-cluster-fleet}"

section() { printf "\n\033[1;36m== %s ==\033[0m\n" "$*"; }

# switch_context <cluster> — set current-context so subsequent kubectl calls
# hit that cluster. GKE mode: gcloud get-credentials. Kind mode: switch to
# the kind-<name> context that `kind create cluster --name <name>` produced.
switch_context() {
  local c="$1"
  if [[ -n "${PROJECT:-}" ]]; then
    : "${REGION:?REGION must be set when PROJECT is set (GKE mode)}"
    gcloud container clusters get-credentials "$c" \
      --region="$REGION" --project="$PROJECT" >/dev/null 2>&1
  else
    kubectl config use-context "kind-$c" >/dev/null 2>&1
  fi
}

fail_modes() {
  local first="${CLUSTERS[0]}"
  local second="${CLUSTERS[1]:-$first}"

  section "FAIL MODE 1 — kill $second's fleet-member, wait 100s, re-apply, expect it out of assignments"
  switch_context "$second"
  kubectl -n "$NS" scale deployment/fleet-member --replicas=0
  echo "sleeping 100s for capacity report to age out (>90s threshold)"
  sleep 100
  fleetctl apply -f "$(dirname "${BASH_SOURCE[0]}")/fleet-spec.yaml"
  fleetctl show-assignments
  echo
  read -rp "press enter to restore $second's fleet-member..."
  switch_context "$second"
  kubectl -n "$NS" scale deployment/fleet-member --replicas=1
  kubectl -n "$NS" rollout status deployment/fleet-member

  section "FAIL MODE 2 — hand-delete a warmpool on $first; fleet-member should recreate it"
  switch_context "$first"
  POOL=$(kubectl -n "$NS" get swp -o jsonpath='{.items[0].metadata.name}')
  echo "deleting $POOL on $first"
  kubectl -n "$NS" delete swp "$POOL" --wait=false
  echo "sleeping 45s for reconcile cycle"
  sleep 45
  echo "after reconcile:"
  kubectl -n "$NS" get swp

  section "FAIL MODE 3 — corrupt assignments.json; fleet-member should log parse error and keep last-known-good"
  : "${FLEET_BUCKET:?FLEET_BUCKET must be set for FAIL MODE 3}"
  echo '{"this":"is not a valid Assignments"}' \
    | gsutil cp - "gs://$FLEET_BUCKET/fleet/assignments.json"
  echo "sleeping 45s; check fleet-member logs for parse error:"
  switch_context "$first"
  kubectl -n "$NS" logs deployment/fleet-member --tail=20 || true
  echo
  read -rp "press enter to restore assignments (re-apply spec)..."
  fleetctl apply -f "$(dirname "${BASH_SOURCE[0]}")/fleet-spec.yaml"
  return 0
}

if [[ "${1:-}" == "--fail-modes" ]]; then
  fail_modes
  exit 0
fi

section "fleetctl status"
fleetctl status || true

for c in "${CLUSTERS[@]}"; do
  switch_context "$c"
  section "[$c] fleet-member pod"
  kubectl -n "$NS" get pods -l app=fleet-member -o wide

  section "[$c] SandboxWarmPools"
  kubectl -n "$NS" get swp -L fleet.agent-sandbox.io/pool

  section "[$c] SandboxTemplates"
  kubectl -n "$NS" get sandboxtemplates.extensions.agents.x-k8s.io
done

section "GCS state"
if [[ -n "${FLEET_BUCKET:-}" ]]; then
  gsutil ls -r "gs://$FLEET_BUCKET/" 2>/dev/null || echo "(bucket empty or no gsutil)"
else
  echo "FLEET_BUCKET not set — skipping GCS inspection"
fi
