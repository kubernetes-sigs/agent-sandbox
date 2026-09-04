#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Phase 4 runbook — the "prove it end-to-end on 3 XS GKE clusters" driver.
#
# What this does (idempotent per phase — re-run safely):
#   Phase 1  Verify prereqs (env vars, gcloud auth, quota estimate).
#   Phase 2  Create 3 XS GKE clusters with --enable-image-streaming.
#            Delegates to deploy/create-gke-standard-fleet.sh.
#   Phase 3  Install agent-sandbox controller + fleet-member on each cluster.
#            Create the GCS bucket + Workload Identity binding.
#   Phase 4  Apply demo/fleet-spec-xs.yaml → warm pools populate.
#            Measure cold wall-clock (time to first fully-warm assignment).
#   Phase 5  Run stress test via FleetSandboxClient. Print summary.
#   Phase 6  (optional --spindown) Scale sandbox pools to 0 to stop billing.
#
# Usage:
#   PROJECT=my-proj FLEET_BUCKET=my-bucket ./demo/run-xs-e2e.sh
#
# Environment variables (all overridable):
#   PROJECT              GCP project           (REQUIRED)
#   FLEET_BUCKET         GCS bucket name       (REQUIRED — created if missing)
#   ZONE                 GKE zone              (default: us-central1-a)
#   CLUSTER_PREFIX       Cluster name prefix   (default: std-multi)
#   N_CLUSTERS           Cluster count         (default: 3)
#   FLEET_MEMBER_IMAGE   Fleet-member image    (default: built + pushed to Artifact Registry)
#   STRESS_RATE          Stress claims/s       (default: 5)
#   STRESS_DURATION      Stress duration (s)   (default: 60)
#   SKIP_STRESS          Set to skip Phase 5   (default: unset)
#   SKIP_TEARDOWN        Set to skip Phase 6   (default: 1 — we don't spin down by default)
#   PHASE                Run only one phase (1|2|3|4|5|6)
#
# Reversibility note:
#   Phase 6 (--spindown) is destructive-ish (scales node pools to zero,
#   halts compute billing). It leaves control planes running (~$0.30/hr).
#   Full teardown: `./deploy/create-gke-standard-fleet.sh delete`.

set -euo pipefail

# --- CONFIG --------------------------------------------------------------

: "${PROJECT:?PROJECT is required — set to your GCP project id}"
: "${FLEET_BUCKET:?FLEET_BUCKET is required — a name for the fleet coordination bucket}"
: "${ZONE:=us-central1-a}"
: "${CLUSTER_PREFIX:=std-multi}"
: "${N_CLUSTERS:=3}"
: "${STRESS_RATE:=5}"
: "${STRESS_DURATION:=60}"
: "${SKIP_TEARDOWN:=1}"

REGION="${ZONE%-*}"  # us-central1-a → us-central1
NS="multi-cluster-fleet"

POC_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$POC_ROOT/.." && pwd)"

GSA_NAME="fleet-agent"
GSA_EMAIL="${GSA_NAME}@${PROJECT}.iam.gserviceaccount.com"
AR_LOCATION="${REGION}"
AR_REPO="fleet-images"

# Default fleet-member image — pushed to your project's Artifact Registry.
: "${FLEET_MEMBER_IMAGE:=${AR_LOCATION}-docker.pkg.dev/${PROJECT}/${AR_REPO}/fleet-member:xs}"

cluster_names() { for i in $(seq 1 "$N_CLUSTERS"); do echo "${CLUSTER_PREFIX}-${i}"; done; }

# --- HELPERS -------------------------------------------------------------

section() { printf "\n\033[1;36m=== %s ===\033[0m\n" "$*"; }
info()    { printf "\033[1;34m[info]\033[0m %s\n" "$*"; }
ok()      { printf "\033[1;32m[ok]\033[0m %s\n" "$*"; }
warn()    { printf "\033[1;33m[warn]\033[0m %s\n" "$*"; }
die()     { printf "\033[1;31m[err]\033[0m %s\n" "$*" >&2; exit 1; }
need()    { command -v "$1" >/dev/null 2>&1 || die "$1 is required on PATH"; }

# --- PHASE 1: Prereqs + quota estimate ----------------------------------

phase1() {
  section "Phase 1 — prereq check"
  need gcloud
  need kubectl
  need docker
  need envsubst
  need python3
  # gsutil is intentionally NOT required — we use `gcloud storage` everywhere.
  # On Compute Engine VMs (esp. Google Corp), gsutil's own credential lookup
  # order (metadata SA → .boto → legacy_credentials) diverges from gcloud's,
  # producing confusing "invalid credentials" errors even when `gcloud auth
  # login` and ADC both work. `gcloud storage` uses the CLI's active
  # credentials — same as any other gcloud command — so there's one auth path
  # to reason about.

  # Auth sanity
  ACCOUNT=$(gcloud config get-value account 2>/dev/null || true)
  [[ -n "$ACCOUNT" ]] || die "no gcloud account set — run 'gcloud auth login'"
  ADC=$(gcloud auth application-default print-access-token >/dev/null 2>&1 \
    && echo "ok" || echo "missing")
  [[ "$ADC" == "ok" ]] || die "no ADC — run 'gcloud auth application-default login'"

  gcloud config set project "$PROJECT" >/dev/null

  # Enable required services
  info "enabling required GCP services on $PROJECT..."
  gcloud services enable container.googleapis.com storage.googleapis.com \
    iam.googleapis.com artifactregistry.googleapis.com --quiet

  # Quota estimate — XS = 3 clusters × 5 e2-standard-8 sandbox (120 vCPU) +
  # 3 × 1 e2-standard-8 controller (24) + 3 × 1 e2-standard-4 control (12) = ~156 vCPU.
  local est=$((N_CLUSTERS * (5*8 + 1*8 + 1*4)))
  info "estimated vCPU need: ~${est} in $ZONE. Checking quota..."
  gcloud compute regions describe "$REGION" --format="value(quotas.metric,quotas.limit)" 2>/dev/null \
    | grep -E "^CPUS " || warn "couldn't read CPUS quota — proceeding anyway"

  ok "prereqs OK for account=$ACCOUNT project=$PROJECT zone=$ZONE"
}

# --- PHASE 2: Clusters --------------------------------------------------

phase2() {
  section "Phase 2 — create 3 XS GKE clusters with image streaming"
  PROJECT="$PROJECT" ZONE="$ZONE" CLUSTER_PREFIX="$CLUSTER_PREFIX" \
    N_CLUSTERS="$N_CLUSTERS" PROFILE=xs USE_GVISOR=no \
    ENABLE_IMAGE_STREAMING=yes \
    "$POC_ROOT/deploy/create-gke-standard-fleet.sh" create
  ok "clusters ready"
}

# --- PHASE 3: Bucket + WI + controller + fleet-member -------------------

phase3() {
  section "Phase 3 — GCS bucket, Workload Identity, controller, fleet-member"

  # 3a. Bucket + service account
  info "ensuring bucket gs://$FLEET_BUCKET exists"
  gcloud storage buckets describe "gs://$FLEET_BUCKET" --project="$PROJECT" \
    >/dev/null 2>&1 \
    || gcloud storage buckets create "gs://$FLEET_BUCKET" \
         --project="$PROJECT" --location="$REGION" --quiet

  info "ensuring GSA $GSA_EMAIL exists"
  gcloud iam service-accounts describe "$GSA_EMAIL" --project="$PROJECT" >/dev/null 2>&1 \
    || gcloud iam service-accounts create "$GSA_NAME" \
         --display-name="Fleet-member GCS access" --project="$PROJECT"

  info "binding storage.objectAdmin on the bucket"
  gcloud storage buckets add-iam-policy-binding "gs://$FLEET_BUCKET" \
    --project="$PROJECT" \
    --member="serviceAccount:$GSA_EMAIL" \
    --role="roles/storage.objectAdmin" \
    --quiet >/dev/null

  # Wipe stale coordination state from any previous session before the fresh
  # fleet-members come up — otherwise they'd read an old assignments.json and
  # create SandboxWarmPools sized to the previous spec, oversubscribing the
  # cluster before Phase 4's apply lands. Silent no-op on a fresh bucket.
  info "wiping stale fleet/ state from prior sessions (safe on fresh buckets)"
  gcloud storage rm "gs://$FLEET_BUCKET/fleet/**" \
    --project="$PROJECT" --quiet 2>/dev/null || true

  # 3b. Build + push fleet-member image (once)
  info "ensuring Artifact Registry repo $AR_REPO exists"
  gcloud artifacts repositories describe "$AR_REPO" \
    --location="$AR_LOCATION" --project="$PROJECT" >/dev/null 2>&1 \
    || gcloud artifacts repositories create "$AR_REPO" \
         --repository-format=docker --location="$AR_LOCATION" --project="$PROJECT"

  if ! gcloud artifacts docker images describe "$FLEET_MEMBER_IMAGE" \
       --project="$PROJECT" >/dev/null 2>&1; then
    info "building fleet-member image $FLEET_MEMBER_IMAGE (context=$REPO_ROOT)"
    gcloud auth configure-docker "${AR_LOCATION}-docker.pkg.dev" --quiet
    docker build -t "$FLEET_MEMBER_IMAGE" \
      -f "$POC_ROOT/deploy/Dockerfile" "$REPO_ROOT"
    docker push "$FLEET_MEMBER_IMAGE"
    ok "pushed $FLEET_MEMBER_IMAGE"
  else
    ok "image $FLEET_MEMBER_IMAGE already exists — skipping build"
  fi

  # 3c. Per-cluster: creds, controller, RBAC, WI, templates, fleet-member
  for c in $(cluster_names); do
    info "priming kubeconfig for $c"
    gcloud container clusters get-credentials "$c" \
      --zone="$ZONE" --project="$PROJECT" >/dev/null 2>&1
    local ctx="gke_${PROJECT}_${ZONE}_${c}"

    info "[$c] installing agent-sandbox v0.5.1 (core + extensions)"
    kubectl --context "$ctx" apply -f \
      "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.1/manifest.yaml" \
      >/dev/null 2>&1 || warn "agent-sandbox core apply returned non-zero (may already be installed)"
    kubectl --context "$ctx" apply -f \
      "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.1/extensions.yaml" \
      >/dev/null 2>&1 || warn "agent-sandbox extensions apply returned non-zero"

    info "[$c] applying fleet RBAC"
    kubectl --context "$ctx" apply -f "$POC_ROOT/deploy/rbac.yaml" >/dev/null

    info "[$c] binding KSA↔GSA via Workload Identity"
    gcloud iam service-accounts add-iam-policy-binding "$GSA_EMAIL" \
      --project="$PROJECT" --role=roles/iam.workloadIdentityUser \
      --member="serviceAccount:${PROJECT}.svc.id.goog[${NS}/fleet-member]" \
      >/dev/null 2>&1 || true
    kubectl --context "$ctx" annotate serviceaccount fleet-member -n "$NS" \
      "iam.gke.io/gcp-service-account=$GSA_EMAIL" --overwrite >/dev/null

    info "[$c] applying XS templates"
    kubectl --context "$ctx" apply -f "$POC_ROOT/deploy/example-templates-xs.yaml" >/dev/null

    info "[$c] deploying fleet-member (WI, controller-pool if present)"
    CLUSTER_NAME="$c" FLEET_MEMBER_IMAGE="$FLEET_MEMBER_IMAGE" \
      FLEET_BUCKET="$FLEET_BUCKET" \
      envsubst < "$POC_ROOT/deploy/fleet-member-deployment-wi.yaml" \
      | kubectl --context "$ctx" apply -f - >/dev/null
    kubectl --context "$ctx" -n "$NS" rollout status deployment/fleet-member --timeout=120s
    ok "[$c] fleet-member Ready"
  done
  ok "Phase 3 complete"
}

# --- PHASE 4: Apply spec, measure cold wall-clock to warm ---------------

phase4() {
  section "Phase 4 — apply XS fleet spec + measure wall-clock to warm"

  info "installing fleetctl (Python planner)"
  python3 -m pip install -qe "$POC_ROOT/python" 2>&1 | tail -1 || true

  # Wait for every fleet-member to have published its first capacity report,
  # otherwise the planner sees zero fresh clusters and produces empty pools.
  info "waiting for capacity reports from all $N_CLUSTERS clusters..."
  local waited=0
  while true; do
    local count
    count=$(gcloud storage ls "gs://$FLEET_BUCKET/fleet/capacity/" \
              --project="$PROJECT" 2>/dev/null | grep -c '\.json$' || true)
    if [[ "$count" -ge "$N_CLUSTERS" ]]; then break; fi
    if [[ $waited -ge 120 ]]; then
      die "only $count/$N_CLUSTERS capacity reports after 120s — check fleet-member logs"
    fi
    sleep 5; waited=$((waited+5))
  done
  ok "all $N_CLUSTERS clusters publishing capacity"

  info "applying $POC_ROOT/demo/fleet-spec-xs.yaml"
  local t0=$(date +%s)
  FLEET_BUCKET="$FLEET_BUCKET" fleetctl apply -f "$POC_ROOT/demo/fleet-spec-xs.yaml" --quiet
  ok "spec applied at t+$(( $(date +%s) - t0 ))s"

  # Target warm total = max_concurrent from the spec.
  local target_warm=300
  info "waiting for wp_ready to reach $target_warm across the fleet..."
  local warm=0
  while true; do
    local total_ready=0
    for c in $(cluster_names); do
      local ctx="gke_${PROJECT}_${ZONE}_${c}"
      local r
      r=$(gcloud storage cat "gs://$FLEET_BUCKET/fleet/capacity/${c}.json" \
            --project="$PROJECT" 2>/dev/null \
          | python3 -c 'import sys,json; print(json.load(sys.stdin).get("warmpool_ready",0))' 2>/dev/null || echo 0)
      total_ready=$((total_ready + r))
    done
    local elapsed=$(( $(date +%s) - t0 ))
    if [[ $total_ready -ge $target_warm ]]; then
      warm=$total_ready
      break
    fi
    if [[ $elapsed -ge 900 ]]; then
      warn "warm=$total_ready/$target_warm after ${elapsed}s — proceeding anyway"
      warm=$total_ready
      break
    fi
    printf "  t+%ds warm=%d/%d\n" "$elapsed" "$total_ready" "$target_warm"
    sleep 15
  done
  local cold_wall_clock=$(( $(date +%s) - t0 ))

  ok "COLD WALL CLOCK to first warm=$warm sandboxes across the fleet: ${cold_wall_clock}s"
  echo
  echo "  → This is the headline metric. It exercises:"
  echo "    (1) planner assignment write → GCS"
  echo "    (2) each fleet-member polling + creating SandboxWarmPools"
  echo "    (3) SandboxWarmPool controller creating sandbox pods"
  echo "    (4) image pull — accelerated by on-node image streaming"
  echo "    (5) pods reaching Ready"
  echo
  echo "  Compare against a single-cluster baseline (3 nodes × 100 pods)"
  echo "  to see the multi-cluster + image-streaming compound win."
  echo
  fleetctl status || true
}

# --- PHASE 5: Stress test with FleetSandboxClient -----------------------

phase5() {
  section "Phase 5 — stress test via FleetSandboxClient (rate=$STRESS_RATE for ${STRESS_DURATION}s)"
  [[ -n "${SKIP_STRESS:-}" ]] && { info "SKIP_STRESS set — skipping"; return 0; }

  PROJECT="$PROJECT" ZONE="$ZONE" FLEET_BUCKET="$FLEET_BUCKET" \
    python3 "$POC_ROOT/demo/stress-e2e.py" \
      --rate "$STRESS_RATE" --duration "$STRESS_DURATION" \
      --per-cluster-concurrency 20 --concurrency 128 \
      --strategy round-robin
}

# --- PHASE 6: Spindown --------------------------------------------------

phase6() {
  section "Phase 6 — sandbox-pool spindown (halts sandbox compute billing)"
  if [[ "$SKIP_TEARDOWN" == "1" ]]; then
    info "SKIP_TEARDOWN=1 (default) — leaving clusters running for interactive work"
    info "run '$POC_ROOT/deploy/create-gke-standard-fleet.sh scale-down' to spindown"
    info "or '$POC_ROOT/deploy/create-gke-standard-fleet.sh delete' to nuke"
    return 0
  fi
  PROJECT="$PROJECT" ZONE="$ZONE" CLUSTER_PREFIX="$CLUSTER_PREFIX" \
    N_CLUSTERS="$N_CLUSTERS" PROFILE=xs \
    "$POC_ROOT/deploy/create-gke-standard-fleet.sh" scale-down
}

# --- DISPATCH -----------------------------------------------------------

case "${PHASE:-all}" in
  1) phase1 ;;
  2) phase2 ;;
  3) phase3 ;;
  4) phase4 ;;
  5) phase5 ;;
  6) phase6 ;;
  all)
    phase1
    phase2
    phase3
    phase4
    phase5
    phase6
    ;;
  *) die "unknown PHASE=$PHASE (use 1|2|3|4|5|6|all)" ;;
esac

section "DONE"
echo "Bucket:      gs://$FLEET_BUCKET"
echo "Clusters:    $(cluster_names | tr '\n' ' ')"
echo "Image:       $FLEET_MEMBER_IMAGE"
echo
echo "Handy follow-ups (--bucket may be passed at either the top level or"
echo "after the subcommand — both work now. Or export FLEET_BUCKET once):"
echo "  export FLEET_BUCKET=$FLEET_BUCKET"
echo "  fleetctl status"
echo "  fleetctl show-assignments"
echo "  fleetctl route --template=sb-tmpl-a --project=$PROJECT --location=$ZONE"
echo "  fleetctl route --all             # phone-book dump: every template + its cluster"
