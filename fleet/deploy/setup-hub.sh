#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Bring up the ClusterProfile hub: identities, IAM, RBAC, ClusterProfile CRs
# and the credential-free hub kubeconfig on every member.
#
# This is the part of Option B (per-member Server-Side Apply) that is pure
# logistics -- one GSA per member, one Workload Identity binding per member,
# one Role per member -- and therefore the part most likely to be done by hand,
# inconsistently, at 6pm. It is scripted so it is repeatable and so
# `--dry-run` can show you the whole blast radius before anything changes.
#
# WHAT IT DOES NOT DO: create the hub cluster. Choose that deliberately.
#
#   Do NOT use one of the density-test clusters. The planner depends on the
#   hub for its inventory, so a hub that also runs a 200k-sandbox test can
#   take out placement for the entire fleet at exactly the moment the fleet is
#   busiest. A small dedicated cluster is enough -- the hub holds a handful of
#   tiny CRs and serves a list every few seconds.
#
# USAGE
#
#   ./deploy/setup-hub.sh \
#     --hub-cluster fleet-hub --hub-location us-central1 \
#     --members cluster-a,cluster-b,cluster-c \
#     --dry-run
#
#   # same command without --dry-run to apply
#
# Idempotent: re-running is how you add a member. Each step tolerates the
# object already existing.
#
# ASSUMPTIONS
#   * Every member cluster has Workload Identity enabled
#     (--workload-pool=PROJECT.svc.id.goog) and the fleet-member namespace and
#     ServiceAccount already exist (deploy/rbac.yaml).
#   * kubectl contexts for the hub and every member are already in your
#     kubeconfig. --context-template controls how member names map to them.
#   * You can create service accounts and set IAM policy in the project.

set -euo pipefail

HUB_CLUSTER=""
HUB_LOCATION=""
MEMBERS=""
# ANNOTATING THE KSA REDIRECTS *EVERY* GOOGLE API CALL THE MEMBER MAKES.
#
# Step 3 points ServiceAccount/fleet-member at a brand-new per-member GSA. That
# annotation is not scoped to the hub -- it changes the identity behind ADC, so
# the member's GCS capacity writes start going out as the new GSA too, and that
# GSA has no bucket access. The member then 403s on GCS, which is a much more
# confusing symptom than "the hub setup broke something".
#
# Pass --bucket to grant the members write access to the fleet bucket at the
# same time. Skipping it is fine only if you are not using the GCS path.
FLEET_BUCKET="${FLEET_BUCKET:-}"
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
HUB_NS="${HUB_NS:-fleet-system}"
MEMBER_NS="${MEMBER_NS:-multi-cluster-fleet}"
KSA="${KSA:-fleet-member}"
PLANNER_KSA="${PLANNER_KSA:-fleet-planner}"
# Which cluster runs the planner. Defaults to the first member.
PLANNER_CLUSTER=""
CLUSTER_MANAGER="${CLUSTER_MANAGER:-agent-sandbox-fleet}"
API_VERSION="${API_VERSION:-v1alpha1}"
CONTEXT_TEMPLATE="${CONTEXT_TEMPLATE:-%s}"
HUB_CONTEXT=""
DRY_RUN=0

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf "\033[1;34m[hub]\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33m[hub] WARN:\033[0m %s\n" "$*" >&2; }
die()  { printf "\033[1;31m[hub] ERROR:\033[0m %s\n" "$*" >&2; exit 1; }

# Every mutating command goes through this, so --dry-run is honest rather than
# "mostly honest". Read-only lookups call the tools directly.
run() {
  if [[ "$DRY_RUN" == "1" ]]; then
    printf "\033[0;90m  would run:\033[0m %s\n" "$*"
  else
    "$@"
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --hub-cluster)      HUB_CLUSTER="$2"; shift 2 ;;
    --hub-location)     HUB_LOCATION="$2"; shift 2 ;;
    --hub-context)      HUB_CONTEXT="$2"; shift 2 ;;
    --members)          MEMBERS="$2"; shift 2 ;;
    --bucket)           FLEET_BUCKET="$2"; shift 2 ;;
    --project)          PROJECT="$2"; shift 2 ;;
    --planner-cluster)  PLANNER_CLUSTER="$2"; shift 2 ;;
    --context-template) CONTEXT_TEMPLATE="$2"; shift 2 ;;
    --cluster-manager)  CLUSTER_MANAGER="$2"; shift 2 ;;
    --api-version)      API_VERSION="$2"; shift 2 ;;
    --dry-run)          DRY_RUN=1; shift ;;
    -h|--help)          sed -n '3,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ -n "$HUB_CLUSTER"  ]] || die "--hub-cluster is required"
[[ -n "$HUB_LOCATION" ]] || die "--hub-location is required"
[[ -n "$MEMBERS"      ]] || die "--members is required (comma-separated)"
[[ -n "$PROJECT"      ]] || die "no project; pass --project"

IFS=',' read -r -a MEMBER_LIST <<< "$MEMBERS"
[[ -n "$HUB_CONTEXT" ]] || HUB_CONTEXT="$(printf "$CONTEXT_TEMPLATE" "$HUB_CLUSTER")"
[[ -n "$PLANNER_CLUSTER" ]] || PLANNER_CLUSTER="${MEMBER_LIST[0]}"

gsa_email() { echo "fleet-member-$1@${PROJECT}.iam.gserviceaccount.com"; }
member_ctx() { printf "$CONTEXT_TEMPLATE" "$1"; }

log "project        $PROJECT"
log "hub            $HUB_CLUSTER ($HUB_LOCATION), context $HUB_CONTEXT, ns $HUB_NS"
log "members        ${MEMBER_LIST[*]}"
log "planner runs on $PLANNER_CLUSTER"
[[ "$DRY_RUN" == "1" ]] && log "DRY RUN — nothing will be changed"

kubectl --context "$HUB_CONTEXT" version -o yaml >/dev/null 2>&1 \
  || die "cannot reach hub context '$HUB_CONTEXT'. Fix --hub-context or --context-template."

# --------------------------------------------------------------------------- #
# 1. ClusterProfile CRD on the hub.
#
# Prefer the real upstream CRD over deploy/clusterprofile-crd.yaml. The fixture
# is preserve-unknown-fields, which means it silently accepts a misspelled
# property name AND makes status.properties atomic under SSA -- so it will pass
# tests that a real hub fails. It exists for kind, not for this.
# --------------------------------------------------------------------------- #
CRD_URL="https://raw.githubusercontent.com/kubernetes-sigs/cluster-inventory-api/main/config/crd/bases/multicluster.x-k8s.io_clusterprofiles.yaml"
log "1/6 installing the upstream ClusterProfile CRD"
if [[ "$DRY_RUN" == "1" ]]; then
  printf "\033[0;90m  would run:\033[0m kubectl --context %s apply -f %s\n" "$HUB_CONTEXT" "$CRD_URL"
else
  kubectl --context "$HUB_CONTEXT" apply -f "$CRD_URL" \
    || die "could not install the CRD from $CRD_URL"
fi
if [[ "$DRY_RUN" == "1" ]]; then
  printf "\033[0;90m  would ensure namespace %s exists on the hub\033[0m\n" "$HUB_NS"
else
  # create|apply rather than plain create, so a re-run is not an error.
  kubectl --context "$HUB_CONTEXT" create namespace "$HUB_NS" \
    --dry-run=client -o yaml | kubectl --context "$HUB_CONTEXT" apply -f -
fi

# --------------------------------------------------------------------------- #
# 2. One GSA per member.
#
# Per-member rather than one shared identity, because the hub Role is scoped
# with resourceNames to a single ClusterProfile. A shared identity would have
# to be allowed to write every profile, and since the planner derives weights
# from sandbox-capacity, any member could then pull the whole fleet's workload
# onto itself by publishing a large number. The isolation is the point.
# --------------------------------------------------------------------------- #
log "2/6 creating one service account per member"
for m in "${MEMBER_LIST[@]}"; do
  email="$(gsa_email "$m")"
  if gcloud iam service-accounts describe "$email" --project "$PROJECT" >/dev/null 2>&1; then
    log "    $email exists"
  else
    run gcloud iam service-accounts create "fleet-member-$m" \
        --project "$PROJECT" \
        --display-name "agent-sandbox fleet member $m"
  fi
done

# --------------------------------------------------------------------------- #
# 3. Workload Identity: KSA -> GSA.
# --------------------------------------------------------------------------- #
log "3/6 binding Workload Identity"
if [[ -z "$FLEET_BUCKET" ]]; then
  warn "no --bucket given: the new per-member GSAs will have NO access to the"
  warn "fleet bucket, and since the KSA annotation below redirects ADC, members"
  warn "using the GCS path will start failing with 403 storage.objects.create."
fi

# The KSA must exist before it can be annotated, and annotating a missing one
# aborts under `set -e` -- after the GSAs and IAM bindings above have already
# been created, leaving a half-configured project. deploy/rbac.yaml creates
# `fleet-member` but `fleet-planner` only appears in planner-deployment.yaml,
# so a member cluster that has not had the planner deployed yet would fail
# here. Ensure both rather than depending on apply order.
ensure_ksa() {
  local ctx="$1" name="$2"
  if kubectl --context "$ctx" get serviceaccount "$name" -n "$MEMBER_NS" \
       >/dev/null 2>&1; then
    return 0
  fi
  if [[ "$DRY_RUN" == "1" ]]; then
    printf "\033[0;90m  would create ServiceAccount %s/%s on %s\033[0m\n" \
        "$MEMBER_NS" "$name" "$ctx"
    return 0
  fi
  kubectl --context "$ctx" create namespace "$MEMBER_NS" \
    --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
  kubectl --context "$ctx" create serviceaccount "$name" -n "$MEMBER_NS"
}

# AUTHENTICATION IS AN IAM CHECK, AUTHORIZATION IS AN RBAC CHECK, AND THEY ARE
# TWO DIFFERENT GRANTS.
#
# The Kubernetes RBAC in step 4 is necessary and not sufficient. Before GKE
# will resolve a Google identity into an RBAC subject at all, that identity
# needs `container.clusters.get` on the project hosting the cluster. A GSA with
# a perfect Role and RoleBinding but no container.* IAM is rejected by the
# apiserver as *unauthenticated*:
#
#   HTTP 401 {"message":"Unauthorized","reason":"Unauthorized","code":401}
#
# which reads as a broken token and sends you off to re-check Workload
# Identity, where you will find nothing wrong. The tell is 401 rather than 403:
# a 403 means the identity resolved and RBAC declined it, so RBAC is the thing
# to look at. A 401 means it never resolved, so look here.
#
# clusterViewer is the smallest role that carries container.clusters.get. It
# grants no access to anything *inside* the cluster -- that still comes from
# the scoped Role in step 4 -- so this stays "IAM to get in the door, RBAC to
# decide what you may touch".
#
# Note that the verification at the bottom of this script cannot catch a
# missing grant here, because `kubectl auth can-i --as=` impersonates from an
# already-authenticated session and so tests only the RBAC half.
grant_cluster_access() {
  run gcloud projects add-iam-policy-binding "$PROJECT" \
      --member "serviceAccount:$1" \
      --role roles/container.clusterViewer \
      --condition=None
}

# Bucket-scoped, not project-scoped: the member writes exactly one object path
# and there is no reason for it to reach the rest of the project's storage.
grant_bucket_access() {
  [[ -n "$FLEET_BUCKET" ]] || return 0
  run gcloud storage buckets add-iam-policy-binding "gs://${FLEET_BUCKET}" \
      --member "serviceAccount:$1" \
      --role roles/storage.objectAdmin
}

for m in "${MEMBER_LIST[@]}"; do
  email="$(gsa_email "$m")"
  run gcloud iam service-accounts add-iam-policy-binding "$email" \
      --project "$PROJECT" \
      --role roles/iam.workloadIdentityUser \
      --member "serviceAccount:${PROJECT}.svc.id.goog[${MEMBER_NS}/${KSA}]" \
      --condition=None
  grant_cluster_access "$email"
  grant_bucket_access "$email"
  ensure_ksa "$(member_ctx "$m")" "$KSA"
  run kubectl --context "$(member_ctx "$m")" annotate serviceaccount "$KSA" \
      -n "$MEMBER_NS" --overwrite \
      "iam.gke.io/gcp-service-account=${email}"
done

# The planner reads every profile, so it gets its own identity with the
# namespace-wide read Role rather than reusing a member's scoped one.
PLANNER_GSA="fleet-planner@${PROJECT}.iam.gserviceaccount.com"
if gcloud iam service-accounts describe "$PLANNER_GSA" --project "$PROJECT" >/dev/null 2>&1; then
  log "    $PLANNER_GSA exists"
else
  run gcloud iam service-accounts create "fleet-planner" \
      --project "$PROJECT" --display-name "agent-sandbox fleet planner"
fi
run gcloud iam service-accounts add-iam-policy-binding "$PLANNER_GSA" \
    --project "$PROJECT" \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:${PROJECT}.svc.id.goog[${MEMBER_NS}/${PLANNER_KSA}]" \
    --condition=None
grant_cluster_access "$PLANNER_GSA"
# The planner reads the registry from GCS and writes placement back, so it
# needs the same bucket access for the same reason the members do.
grant_bucket_access "$PLANNER_GSA"
ensure_ksa "$(member_ctx "$PLANNER_CLUSTER")" "$PLANNER_KSA"
run kubectl --context "$(member_ctx "$PLANNER_CLUSTER")" \
    annotate serviceaccount "$PLANNER_KSA" -n "$MEMBER_NS" --overwrite \
    "iam.gke.io/gcp-service-account=${PLANNER_GSA}"

# --------------------------------------------------------------------------- #
# 4. Hub RBAC.
#
# GKE maps an IAM identity onto an RBAC subject of kind User named by the GSA
# email, which is why gen-hub-rbac.py defaults to exactly that.
# --------------------------------------------------------------------------- #
log "4/6 applying per-member hub RBAC"
RBAC_YAML="$(python3 "$HERE/gen-hub-rbac.py" \
  --namespace "$HUB_NS" --project "$PROJECT" \
  --planner-subject "$PLANNER_GSA" \
  --clusters "${MEMBER_LIST[@]}")"
if [[ "$DRY_RUN" == "1" ]]; then
  printf "\033[0;90m  would apply %s lines of RBAC to %s\033[0m\n" \
      "$(wc -l <<< "$RBAC_YAML")" "$HUB_CONTEXT"
else
  kubectl --context "$HUB_CONTEXT" apply -f - <<< "$RBAC_YAML"
fi

# --------------------------------------------------------------------------- #
# 5. A ClusterProfile per member.
#
# Members apply only `status` and only for their own name; nobody is allowed to
# `create`, precisely so a compromised member cannot invent clusters. So the
# objects have to exist first, created here by the operator.
# --------------------------------------------------------------------------- #
log "5/6 creating a ClusterProfile per member"
for m in "${MEMBER_LIST[@]}"; do
  profile=$(cat <<YAML
apiVersion: multicluster.x-k8s.io/${API_VERSION}
kind: ClusterProfile
metadata:
  name: ${m}
  namespace: ${HUB_NS}
  labels:
    x-k8s.io/cluster-manager: ${CLUSTER_MANAGER}
spec:
  displayName: ${m}
  clusterManager:
    name: ${CLUSTER_MANAGER}
YAML
)
  if [[ "$DRY_RUN" == "1" ]]; then
    printf "\033[0;90m  would create ClusterProfile %s\033[0m\n" "$m"
  else
    kubectl --context "$HUB_CONTEXT" apply -f - <<< "$profile"
  fi
done

# --------------------------------------------------------------------------- #
# 6. The credential-free hub kubeconfig, on every member and on the planner.
# --------------------------------------------------------------------------- #
log "6/6 distributing the hub kubeconfig ConfigMap"
CM_YAML="$("$HERE/gen-hub-kubeconfig.sh" \
  --hub-cluster "$HUB_CLUSTER" --hub-location "$HUB_LOCATION" \
  --project "$PROJECT" --namespace "$MEMBER_NS")"
for m in "${MEMBER_LIST[@]}"; do
  if [[ "$DRY_RUN" == "1" ]]; then
    printf "\033[0;90m  would apply hub-kubeconfig ConfigMap to %s\033[0m\n" "$(member_ctx "$m")"
  else
    kubectl --context "$(member_ctx "$m")" apply -f - <<< "$CM_YAML"
  fi
done

# --------------------------------------------------------------------------- #
# Verification. Publishing is wrapped in try/except so the GCS path survives a
# broken hub -- which also means every misconfiguration here is SILENT. These
# checks are not optional politeness.
# --------------------------------------------------------------------------- #
if [[ "$DRY_RUN" == "1" ]]; then
  log "dry run complete — nothing was changed"
  exit 0
fi

log "verifying RBAC scoping"

# `kubectl auth can-i` answers on STDOUT and exits 1 for "no", so a non-zero
# exit is a normal answer here, not an error -- and `|| echo no` would both
# duplicate the word and, worse, turn a genuine failure (unreachable hub,
# missing RBAC) into a clean-looking "no" that reads as correct scoping.
# Distinguish the three outcomes instead: yes, no, and ERROR.
can_i() {
  local as="$1"; shift
  local out err rc
  err=$(mktemp)
  out=$(kubectl --context "$HUB_CONTEXT" auth can-i "$@" \
        -n "$HUB_NS" --as="$as" 2>"$err") && rc=0 || rc=$?
  out="${out%%$'\n'*}"
  if [[ "$out" != "yes" && "$out" != "no" ]]; then
    warn "auth can-i did not answer (rc=$rc): $(tr '\n' ' ' <"$err")"
    out="ERROR"
  fi
  rm -f "$err"
  echo "$out"
}

fail=0
for m in "${MEMBER_LIST[@]}"; do
  as="$(gsa_email "$m")"
  own=$(can_i    "$as" patch "clusterprofiles/$m" --subresource=status)
  create=$(can_i "$as" create clusterprofiles)
  # Peer check only means something with more than one member.
  peer="n/a"
  for other in "${MEMBER_LIST[@]}"; do
    [[ "$other" == "$m" ]] && continue
    peer=$(can_i "$as" patch "clusterprofiles/$other" --subresource=status)
    break
  done
  printf "    %-16s own=%-6s peer=%-6s create=%-6s\n" "$m" "$own" "$peer" "$create"
  [[ "$own"    == "yes" ]] || { warn "$m cannot patch its OWN status"; fail=1; }
  [[ "$peer"   == "yes" ]] && { warn "$m CAN patch a peer's status — scoping is broken"; fail=1; }
  [[ "$create" == "yes" ]] && { warn "$m can create ClusterProfiles — scoping is broken"; fail=1; }
  # An unanswered check proves nothing; treat it as a failure rather than
  # letting it pass for looking like "no".
  [[ "$own$peer$create" == *ERROR* ]] && { warn "$m: a check did not run"; fail=1; }
done
[[ "$fail" == "0" ]] || die "RBAC verification failed; do not enable publishing yet"

cat <<EOF

$(log "hub is ready")

Next, in this order — the order matters:

  1. Turn on publishing, one member at a time:

       CLUSTER=${MEMBER_LIST[0]}
       sed "s/\\\${CLUSTER_NAME}/\$CLUSTER/g; s/\\\${FLEET_BUCKET}/\$FLEET_BUCKET/g; \\
            s/\\\${SANDBOX_CAPACITY}/200000/g" \\
         deploy/fleet-member-clusterprofile-patch.yaml \\
         | kubectl --context \$(printf "$CONTEXT_TEMPLATE" "\$CLUSTER") \\
             patch deployment fleet-member -n $MEMBER_NS --patch-file /dev/stdin

  2. Confirm the applies own their fields. managedFields is the ground truth —
     a merge-patch would land the data and own nothing:

       kubectl --context $HUB_CONTEXT get clusterprofile \$CLUSTER -n $HUB_NS \\
         -o jsonpath='{.metadata.managedFields[*].manager}'

     'agent-sandbox-fleet-member' must appear.

  3. Only once EVERY member shows capacity, switch the planner over:

       kubectl --context $HUB_CONTEXT get clusterprofile -n $HUB_NS -o json \\
         | jq -r '.items[] | .metadata.name + " " +
             ((.status.properties[]? | select(.name=="agents.x-k8s.io/sandbox-capacity") | .value) // "MISSING")'

     Any MISSING row means the planner would see a capacity-less fleet.
EOF
