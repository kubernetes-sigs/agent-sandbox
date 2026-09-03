#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Render a fleet manifest with envsubst, refusing to emit one that has a hole
# in it.
#
# WHY THIS EXISTS
#   envsubst replaces an unset variable with the empty string and says nothing.
#   That is fine for a value nobody reads and catastrophic for a container arg:
#
#     --loop-interval=${LOOP_INTERVAL}   unset  ->  --loop-interval=
#
#   argparse rejects an empty float, so the planner exits before it logs a
#   single line. `kubectl rollout status` had already returned success and
#   `kubectl logs` printed nothing at all, which reads like a silent hang
#   rather than a crash. The quieter version of the same bug is worse:
#
#     --cluster-manager=${CLUSTER_MANAGER}  unset  ->  --cluster-manager=
#
#   an empty string is falsy, so the planner drops its label selector and
#   inventories every ClusterProfile on the hub, including other fleets'. That
#   one starts cleanly and is wrong.
#
#   Several manifests document defaults in their header comments ("default
#   60"). envsubst has no default syntax, so those defaults existed only in
#   prose. They are implemented here.
#
# USAGE
#   ./deploy/render.sh deploy/planner-deployment.yaml | kubectl apply -f -
#   kubectl patch deployment fleet-planner -n multi-cluster-fleet \
#     --patch "$(./deploy/render.sh deploy/planner-clusterprofile-patch.yaml)"
#
#   Anything already exported wins; these are floors, not overrides.
set -euo pipefail

die() { echo "render.sh: $*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: render.sh <manifest.yaml>"
FILE="$1"
[[ -f "$FILE" ]] || die "no such file: $FILE"
command -v envsubst >/dev/null 2>&1 || die "envsubst not found (apt install gettext-base)"

# Documented defaults, kept in sync with the manifest header comments. Only
# values with a sane fleet-wide default belong here -- a bucket or an image
# must never be guessed.
: "${LOOP_INTERVAL:=60}"
: "${CLUSTER_MANAGER:=agent-sandbox-fleet}"   # matches setup-hub.sh
: "${HUB_NAMESPACE:=fleet-system}"
: "${SANDBOX_CAPACITY:=200}"
export LOOP_INTERVAL CLUSTER_MANAGER HUB_NAMESPACE SANDBOX_CAPACITY

# Everything the file actually references, minus what is now set.
missing=()
while read -r var; do
  [[ -n "${!var:-}" ]] || missing+=("$var")
done < <(grep -oh '\${[A-Za-z_][A-Za-z0-9_]*}' "$FILE" | tr -d '${}' | sort -u)

if [[ ${#missing[@]} -gt 0 ]]; then
  die "$FILE needs these, and they are unset: ${missing[*]}
     export them and re-run; see the manifest header for what each one means."
fi

rendered="$(envsubst < "$FILE")"

# Belt and braces: catch a blank arg even if it came from a variable that was
# exported as the empty string, which the loop above cannot distinguish from
# an intentional value.
if blank="$(grep -n -- '^[[:space:]]*-[[:space:]]*--[a-z-]*=$' <<<"$rendered")"; then
  die "$FILE rendered a container arg with no value:
$blank
     an empty flag is silently accepted by some parsers and fatal to others."
fi

printf '%s\n' "$rendered"
