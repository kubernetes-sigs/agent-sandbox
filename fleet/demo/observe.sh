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
#   ZONE      GKE zone for ZONAL clusters (Standard, e.g. us-central1-a).
#   REGION    GKE region for REGIONAL clusters (Autopilot, e.g. us-central1).
#             Exactly one of ZONE/REGION is required when PROJECT is set; ZONE
#             wins if both are set. The XS driver builds zonal clusters, so
#             --region alone silently fails to find them.
#   FLEET_BUCKET  GCS bucket for the fleet hub (used in --fail-modes + summary)
#   FLEET_SPEC    Path to the spec to re-apply in --fail-modes. REQUIRED for
#                 --fail-modes; see the note on fail_modes() below.
#
# Examples:
#   ./demo/observe.sh                                      # kind 2-cluster demo
#   CLUSTERS="cluster-a cluster-b cluster-c" \
#     PROJECT=my-proj REGION=us-central1 ./demo/observe.sh # regional (Autopilot)
#   CLUSTERS="std-multi-1 std-multi-2 std-multi-3" \
#     PROJECT=my-proj ZONE=us-central1-a ./demo/observe.sh # zonal (XS driver)
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
    # A zonal cluster is not found by --region and vice versa; get-credentials
    # exits non-zero and, because the call is >/dev/null 2>&1, every kubectl
    # below would then silently report on whatever context happened to be
    # current. Pick the flag from whichever of ZONE/REGION is set, and let the
    # failure surface.
    local loc_flag loc_value
    if [[ -n "${ZONE:-}" ]]; then
      loc_flag="--zone"; loc_value="$ZONE"
    elif [[ -n "${REGION:-}" ]]; then
      loc_flag="--region"; loc_value="$REGION"
    else
      echo "ZONE or REGION must be set when PROJECT is set (GKE mode)" >&2
      exit 1
    fi
    gcloud container clusters get-credentials "$c" \
      "$loc_flag=$loc_value" --project="$PROJECT" >/dev/null 2>&1 \
      || { echo "get-credentials failed for $c ($loc_flag=$loc_value)." \
                "Zonal clusters need ZONE, regional need REGION." >&2; exit 1; }
  else
    kubectl config use-context "kind-$c" >/dev/null 2>&1
  fi
}

# --fail-modes deliberately breaks a live fleet and then puts it back. Between
# those two points the script can end without reaching the recovery: a failed
# `fleetctl apply`, switch_context exiting on a get-credentials failure, `read`
# hitting EOF under `set -e`, or -- much the likeliest -- the operator pressing
# Ctrl-C at one of the two prompts, which are the only places this script waits.
# Any of those leaves the fleet injected and says nothing about it. These two
# variables record what is currently broken, and restore() runs off a trap so
# the recovery is not on the happy path.
_RESTORE_MEMBER=""   # cluster whose fleet-member is scaled to 0
_RESTORE_ASSIGN=""   # spec to re-apply while assignments.json is corrupt

restore() {
  local rc=$?
  trap - EXIT INT TERM      # never re-enter, whatever happens below
  [[ -n "$_RESTORE_MEMBER" || -n "$_RESTORE_ASSIGN" ]] || exit "$rc"

  section "restoring injected fail-mode state"
  local failed=""
  if [[ -n "$_RESTORE_MEMBER" ]]; then
    # switch_context exits 1 on a get-credentials failure, which from inside a
    # trap would abandon the assignments restore below. Run it in a subshell so
    # it cannot -- which still does the job, because both of its branches write
    # the current context to the kubeconfig FILE, not to shell state.
    if ( switch_context "$_RESTORE_MEMBER" ) \
       && kubectl -n "$NS" scale deployment/fleet-member --replicas=1; then
      _RESTORE_MEMBER=""
    else
      failed=yes
    fi
  fi
  if [[ -n "$_RESTORE_ASSIGN" ]]; then
    if fleetctl apply -f "$_RESTORE_ASSIGN"; then
      _RESTORE_ASSIGN=""
    else
      failed=yes
    fi
  fi

  if [[ -n "$failed" ]]; then
    printf "\n\033[1;31m== COULD NOT RESTORE — the fleet is STILL broken ==\033[0m\n" >&2
    [[ -z "$_RESTORE_MEMBER" ]] || printf '%s\n' \
      "  fleet-member on $_RESTORE_MEMBER is still scaled to 0. Point kubectl" \
      "  at $_RESTORE_MEMBER and run:" \
      "    kubectl -n $NS scale deployment/fleet-member --replicas=1" >&2
    [[ -z "$_RESTORE_ASSIGN" ]] || printf '%s\n' \
      "  gs://${FLEET_BUCKET:-<bucket>}/fleet/assignments.json is still the" \
      "  corrupted placeholder; every member is running on last-known-good." \
      "  Recover with:" \
      "    fleetctl apply -f $_RESTORE_ASSIGN" >&2
    exit 1
  fi
  exit "$rc"
}

fail_modes() {
  local first="${CLUSTERS[0]}"
  local second="${CLUSTERS[1]:-$first}"

  # Preflight EVERYTHING before the first mutation. FAIL MODE 1 scales
  # fleet-member to zero and then sleeps 100s; discovering a missing spec or an
  # absent fleetctl after that point leaves the member down and the operator
  # holding a half-injected fleet.
  #
  # FLEET_SPEC has no default on purpose. This script used to hardcode
  # `fleet-spec.yaml` next to itself, so running it against a fleet built from
  # any other spec (the XS driver uses fleet-spec-xs.yaml) did not restore the
  # fleet -- it silently REPLACED the live assignment with a different plan, and
  # the "recovery" the fail mode claims to demonstrate was a re-plan of
  # something else. Naming the spec is cheap; guessing it is not.
  : "${FLEET_SPEC:?FLEET_SPEC must be set for --fail-modes -- the path to the
     SAME spec this fleet was built from (e.g. demo/fleet-spec-xs.yaml).
     Fail modes 1 and 3 re-apply it to recover; applying a different spec
     replaces the assignment instead of restoring it.}"
  [[ -f "$FLEET_SPEC" ]] || { echo "FLEET_SPEC=$FLEET_SPEC does not exist" >&2; exit 1; }
  : "${FLEET_BUCKET:?FLEET_BUCKET must be set for --fail-modes (FAIL MODE 3)}"
  command -v fleetctl >/dev/null 2>&1 \
    || { echo "fleetctl is not on PATH — 'pip install -e ./python' first" >&2; exit 1; }

  # Armed only after the preflight above, so a bad invocation exits before
  # anything is owed.
  trap restore EXIT INT TERM

  section "FAIL MODE 1 — kill $second's fleet-member, wait 100s, re-apply, expect it out of assignments"
  switch_context "$second"
  _RESTORE_MEMBER="$second"
  kubectl -n "$NS" scale deployment/fleet-member --replicas=0
  echo "sleeping 100s for capacity report to age out (>90s threshold)"
  sleep 100
  fleetctl apply -f "$FLEET_SPEC"
  fleetctl show-assignments
  echo
  read -rp "press enter to restore $second's fleet-member..."
  switch_context "$second"
  kubectl -n "$NS" scale deployment/fleet-member --replicas=1
  _RESTORE_MEMBER=""
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
  # Context first. switch_context exits on a get-credentials failure, and there
  # is no reason for that to happen with the bucket already corrupted.
  switch_context "$first"
  _RESTORE_ASSIGN="$FLEET_SPEC"
  echo '{"this":"is not a valid Assignments"}' \
    | gcloud storage cp - "gs://$FLEET_BUCKET/fleet/assignments.json"
  echo "sleeping 45s; check fleet-member logs for parse error:"
  sleep 45
  kubectl -n "$NS" logs deployment/fleet-member --tail=20 || true
  echo
  read -rp "press enter to restore assignments (re-apply spec)..."
  fleetctl apply -f "$FLEET_SPEC"
  _RESTORE_ASSIGN=""
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
  gcloud storage ls -r "gs://$FLEET_BUCKET/**" 2>/dev/null \
    || echo "(bucket empty, or no access with the current gcloud credentials)"
else
  echo "FLEET_BUCKET not set — skipping GCS inspection"
fi
