#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Create N GKE Standard clusters for fleet-at-scale testing. Modeled on
# GKE's internal agent-sandbox perf-test cluster configs (5k-project and
# 1k-project profiles) — dedicated controller pool, NodeLocalDNS, DPv2,
# large pod CIDR — so the fleet story can be proven at production-relevant scale.
#
# Each cluster gets:
#   - control-plane node pool (small, hosts kube-system)
#   - controller-pool: 1× e2-standard-32, TAINTED, hosts agent-sandbox-controller
#     and fleet-member. Isolated from workload contention.
#   - sandbox-pool: N × <machine>, hosts SandboxWarmPool pods.
#     Tainted with sandbox.gke.io/runtime=gvisor if USE_GVISOR=yes.
#   - Workload Identity enabled
#   - Cluster IPv4 CIDR /11 (matches perf-test — 2M pod IPs for headroom)
#   - NodeLocalDNS + Dataplane V2 addons (perf-test defaults)
#
# COST WARNING — 3-cluster fleet, fully loaded:
#   XS  (fits personal quota, dev proof): ~$8/hr   (15 e2-standard-8 sandbox nodes total)
#   S   (15k fleet-wide, 5k/cluster):    ~$65/hr
#   M   (45k, 15k/cluster):              ~$100/hr
#   L   (150k, 50k/cluster):             ~$320/hr
#   XL  (300k, 100k/cluster):            ~$300/hr  (n2-standard-4 is efficient)
#   XXL (555k, 185k/cluster):            ~$450/hr  (matches published ceiling)
# Tear down between test runs — scale-down mode keeps only control planes at ~$0.30/hr.
#
# IMAGE STREAMING is enabled on all sandbox pools by default via
# --enable-image-streaming. This turns on GKE's on-node gcfsd-v2 daemon so
# containers start on layer-stream reads instead of full-pull-then-run. Combined
# with placement_policy=image-affinity in the fleet spec (same image always
# lands on same cluster), this is the joint fleet + image-streaming story.
#
# Modes:
#   ./create-gke-standard-fleet.sh              # default: create
#   ./create-gke-standard-fleet.sh create
#   ./create-gke-standard-fleet.sh scale-down   # sandbox pool → 0 nodes
#   ./create-gke-standard-fleet.sh scale-up     # sandbox pool → configured size
#   ./create-gke-standard-fleet.sh delete       # nuke everything (irreversible)
#   ./create-gke-standard-fleet.sh status
#
# Env vars:
#   PROJECT               GCP project             (required)
#   ZONE                  GKE zone                (default: us-central1-a)
#   CLUSTER_PREFIX        Cluster name prefix     (default: std-multi)
#   N_CLUSTERS            How many               (default: 3)
#   PROFILE               xs|s|m|l|xl|xxl or custom (default: xs)
#                         xs  =   100 warm/cluster, 5 e2-standard-8 sandbox nodes  (dev proof, fits personal quota)
#                         s   =  5k warm/cluster, 40 e2-standard-16 sandbox nodes
#                         m   = 15k warm/cluster, 100 e2-standard-8 sandbox nodes  (matches perf-test burst)
#                         l   = 50k warm/cluster, 250 e2-standard-8 sandbox nodes
#                         xl  = 100k warm/cluster, 400 n2-standard-4 sandbox nodes
#                         xxl = 185k warm/cluster, 750 n2-standard-4 sandbox nodes (matches perf-test capacity-cliff)
#   USE_GVISOR            "yes" or "no"          (default: no)
#                         "yes" requires SandboxTemplate to have runtimeClassName: gvisor
#                         + toleration; see deploy/example-templates.yaml.
#
# Custom-profile env vars (override the profile defaults):
#   CONTROL_NODE_COUNT    Default kube-system pool size    (default: 3)
#   CONTROL_MACHINE       Default pool machine type        (default: e2-standard-4)
#   CONTROLLER_MACHINE    Controller pool machine type     (default: e2-standard-32)
#   SANDBOX_NODE_COUNT    Sandbox pool size                (per PROFILE)
#   SANDBOX_MACHINE       Sandbox pool machine type        (per PROFILE)
#   MAX_PODS_PER_NODE     Sandbox pool max pods            (per PROFILE)
set -euo pipefail

: "${PROJECT:?PROJECT env var is required}"
: "${ZONE:=us-central1-a}"
: "${CLUSTER_PREFIX:=std-multi}"
: "${N_CLUSTERS:=3}"
: "${PROFILE:=xs}"
: "${USE_GVISOR:=no}"
# Image streaming — on by default. Set to "no" to A/B against baseline.
: "${ENABLE_IMAGE_STREAMING:=yes}"

# Static defaults per profile — set BEFORE the case block so the XS-specific
# override below is authoritative when PROFILE=xs. `:=` only assigns if
# unset/null, so we must not pre-set these before per-profile decisions.
if [[ "${PROFILE:-}" == "xs" ]]; then
  # XS defaults — smaller everything to fit personal-project quota.
  : "${CONTROL_NODE_COUNT:=1}"
  : "${CONTROL_MACHINE:=e2-standard-4}"
  : "${CONTROLLER_MACHINE:=e2-standard-8}"
else
  # S..XXL defaults — production-sized isolation and control-plane headroom.
  : "${CONTROL_NODE_COUNT:=3}"
  : "${CONTROL_MACHINE:=e2-standard-4}"
  : "${CONTROLLER_MACHINE:=e2-standard-32}"
fi

# Profile-driven defaults (env-var override wins).
#
# CLUSTER_IPV4_CIDR sizing note: gcloud reserves the whole block per cluster
# from the VPC's private space. Three clusters at /11 (~2M pod IPs each) do
# not fit in the default network's 10.0.0.0/9 pool — hence per-profile sizing
# to just what each rung needs. Formula: pod slots = SANDBOX_NODE_COUNT ×
# MAX_PODS_PER_NODE × 1.5 headroom, rounded UP to the next /N.
case "$PROFILE" in
  # XS — dev proof rung. 3 clusters × 5 e2-standard-8 = 120 sandbox vCPUs;
  # controller pools (3 × e2-standard-8 = 24) + control (3 × 1 × e2-standard-4 = 12)
  # = ~156 total vCPUs. Fits under 500-vCPU personal-project quota with headroom.
  # Warm target ~100 per cluster. Pod slot need: 5 × 110 × 1.5 = 825 → /21 (2k) fits.
  xs)  : "${SANDBOX_NODE_COUNT:=5}";    : "${SANDBOX_MACHINE:=e2-standard-8}";  : "${MAX_PODS_PER_NODE:=110}"; : "${CLUSTER_IPV4_CIDR:=/21}"; ;;
  # S — 40 × e2-standard-16, 256 pods/node = 10k slots × 1.5 = 15k → /18 (16k) fits.
  s)   : "${SANDBOX_NODE_COUNT:=40}";   : "${SANDBOX_MACHINE:=e2-standard-16}"; : "${MAX_PODS_PER_NODE:=256}"; : "${CLUSTER_IPV4_CIDR:=/18}"; ;;
  # M — 100 × e2-standard-8, 150 pods/node = 15k slots × 1.5 = 22.5k → /17 (32k) fits.
  m)   : "${SANDBOX_NODE_COUNT:=100}";  : "${SANDBOX_MACHINE:=e2-standard-8}";  : "${MAX_PODS_PER_NODE:=150}"; : "${CLUSTER_IPV4_CIDR:=/17}"; ;;
  # L — 250 × e2-standard-8, 200 pods/node = 50k slots × 1.5 = 75k → /15 (128k) fits.
  l)   : "${SANDBOX_NODE_COUNT:=250}";  : "${SANDBOX_MACHINE:=e2-standard-8}";  : "${MAX_PODS_PER_NODE:=200}"; : "${CLUSTER_IPV4_CIDR:=/15}"; ;;
  # XL — 400 × n2-standard-4, 256 pods/node = 102k slots × 1.5 = 153k → /13 (512k) fits.
  xl)  : "${SANDBOX_NODE_COUNT:=400}";  : "${SANDBOX_MACHINE:=n2-standard-4}";  : "${MAX_PODS_PER_NODE:=256}"; : "${CLUSTER_IPV4_CIDR:=/13}"; ;;
  # XXL — 750 × n2-standard-4, 256 pods/node = 192k slots × 1.5 = 288k → /11 (2M) matches perf-test.
  xxl) : "${SANDBOX_NODE_COUNT:=750}";  : "${SANDBOX_MACHINE:=n2-standard-4}";  : "${MAX_PODS_PER_NODE:=256}"; : "${CLUSTER_IPV4_CIDR:=/11}"; ;;
  *) echo "unknown PROFILE=$PROFILE; use xs|s|m|l|xl|xxl or override SANDBOX_* env vars"; exit 2 ;;
esac

MODE="${1:-create}"
SANDBOX_POOL_NAME="sandbox-pool"
CONTROLLER_POOL_NAME="controller-pool"

# Taints
CONTROLLER_TAINT="controller-pool=true:NoSchedule"
SANDBOX_GVISOR_TAINT="sandbox.gke.io/runtime=gvisor:NoSchedule"  # only if USE_GVISOR=yes

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

section() { printf "\n\033[1;36m== %s ==\033[0m\n" "$*"; }
note()    { printf "\033[1;33m[note]\033[0m %s\n" "$*"; }
ok()      { printf "\033[1;32m[ok]\033[0m %s\n" "$*"; }
err()     { printf "\033[1;31m[err]\033[0m %s\n" "$*" >&2; }

cluster_names() { for i in $(seq 1 "$N_CLUSTERS"); do echo "${CLUSTER_PREFIX}-${i}"; done; }

cluster_exists() { gcloud container clusters describe "$1" --zone="$ZONE" --project="$PROJECT" >/dev/null 2>&1; }
pool_exists()    { gcloud container node-pools describe "$2" --cluster="$1" --zone="$ZONE" --project="$PROJECT" >/dev/null 2>&1; }

# cluster_status <name> → prints RUNNING | ERROR | PROVISIONING | STOPPING | (empty if absent).
# Used to distinguish "cluster is fine, skip create" from "cluster is broken,
# needs to be deleted before we can retry" — the second case would otherwise
# leave the script trying to attach node pools to an ERROR cluster.
cluster_status() {
  gcloud container clusters describe "$1" \
    --zone="$ZONE" --project="$PROJECT" \
    --format='value(status)' 2>/dev/null || true
}

# --------------------------------------------------------------------------- #
# Create one cluster + controller pool + sandbox pool. Backgrounded from main.
# --------------------------------------------------------------------------- #

create_one() {
  local c="$1"
  local log="/tmp/create-${c}.log"

  if cluster_exists "$c"; then
    local status
    status=$(cluster_status "$c")
    if [[ "$status" == "ERROR" || "$status" == "DEGRADED" ]]; then
      err "cluster $c exists but STATUS=$status — cannot proceed."
      err "Delete it first: gcloud container clusters delete $c --zone=$ZONE --project=$PROJECT --quiet"
      return 1
    fi
    if [[ "$status" != "RUNNING" ]]; then
      err "cluster $c is in STATUS=$status; wait for it to settle before re-running"
      return 1
    fi
    ok "cluster $c already exists — skipping create"
  else
    printf "  [%s] creating cluster (~5-8 min)... log: %s\n" "$c" "$log"
    # --cluster-ipv4-cidr: per-profile size (see the case block above). At
    #   /11 the default VPC's 10.0.0.0/9 fits only ~2 clusters, so XS-L use
    #   smaller blocks matched to their actual pod-slot need.
    # --addons=NodeLocalDNS: reduces kube-dns load at scale
    # --enable-dataplane-v2: eBPF networking, much faster than iptables at 1k+ nodes
    # --workload-pool: enables Workload Identity for GCS auth without key files
    # --release-channel=rapid: matches perf-test config
    gcloud container clusters create "$c" \
      --project="$PROJECT" \
      --zone="$ZONE" \
      --addons=NodeLocalDNS \
      --enable-ip-alias \
      --enable-dataplane-v2 \
      --cluster-ipv4-cidr="$CLUSTER_IPV4_CIDR" \
      --scopes="https://www.googleapis.com/auth/cloud-platform" \
      --num-nodes="$CONTROL_NODE_COUNT" \
      --machine-type="$CONTROL_MACHINE" \
      --workload-pool="$PROJECT.svc.id.goog" \
      --release-channel=rapid \
      --quiet \
      >"$log" 2>&1
    ok "cluster $c created"
  fi

  # Controller pool — 1× beefy node, tainted, dedicated to agent-sandbox-controller + fleet-member
  if pool_exists "$c" "$CONTROLLER_POOL_NAME"; then
    ok "node pool $c/$CONTROLLER_POOL_NAME already exists — skipping"
  else
    local pool_log="/tmp/pool-${c}-controller.log"
    printf "  [%s] creating controller pool (1× %s, tainted)... log: %s\n" "$c" "$CONTROLLER_MACHINE" "$pool_log"
    gcloud container node-pools create "$CONTROLLER_POOL_NAME" \
      --cluster="$c" --project="$PROJECT" --zone="$ZONE" \
      --num-nodes=1 \
      --machine-type="$CONTROLLER_MACHINE" \
      --image-type=cos_containerd \
      --node-taints="$CONTROLLER_TAINT" \
      --quiet \
      >"$pool_log" 2>&1
    ok "node pool $c/$CONTROLLER_POOL_NAME created"
  fi

  # Sandbox pool — the workhorse
  if pool_exists "$c" "$SANDBOX_POOL_NAME"; then
    ok "node pool $c/$SANDBOX_POOL_NAME already exists — skipping"
  else
    local pool_log="/tmp/pool-${c}-sandbox.log"
    printf "  [%s] creating sandbox pool (%d× %s, %d pods/node)... log: %s\n" \
      "$c" "$SANDBOX_NODE_COUNT" "$SANDBOX_MACHINE" "$MAX_PODS_PER_NODE" "$pool_log"

    local gvisor_flag=""
    if [[ "$USE_GVISOR" == "yes" ]]; then
      gvisor_flag="--sandbox type=gvisor"
    fi
    # Image Streaming — see gcloud docs.
    # Delegates layer reads to on-node gcfsd-v2 so the container starts on
    # first-read instead of waiting for the full pull. Compounds with
    # placement_policy=image-affinity in the fleet spec: same image lands on
    # same cluster → cache stays hot across claims.
    local streaming_flag=""
    if [[ "$ENABLE_IMAGE_STREAMING" == "yes" ]]; then
      streaming_flag="--enable-image-streaming"
    fi

    # shellcheck disable=SC2086
    gcloud container node-pools create "$SANDBOX_POOL_NAME" \
      --cluster="$c" --project="$PROJECT" --zone="$ZONE" \
      --num-nodes="$SANDBOX_NODE_COUNT" \
      --machine-type="$SANDBOX_MACHINE" \
      --image-type=cos_containerd \
      --max-pods-per-node="$MAX_PODS_PER_NODE" \
      $gvisor_flag \
      $streaming_flag \
      --quiet \
      >"$pool_log" 2>&1
    ok "node pool $c/$SANDBOX_POOL_NAME created (image_streaming=$ENABLE_IMAGE_STREAMING)"
  fi
}

# --------------------------------------------------------------------------- #
# Modes
# --------------------------------------------------------------------------- #

do_create() {
  section "Creating $N_CLUSTERS clusters (profile=$PROFILE, gvisor=$USE_GVISOR, image_streaming=$ENABLE_IMAGE_STREAMING)"
  printf "  cluster names: %s\n" "$(cluster_names | tr '\n' ' ')"
  printf "  control:    %d × %s per cluster\n" "$CONTROL_NODE_COUNT" "$CONTROL_MACHINE"
  printf "  controller: 1 × %s per cluster (TAINTED: %s)\n" "$CONTROLLER_MACHINE" "$CONTROLLER_TAINT"
  printf "  sandbox:    %d × %s per cluster (%d pods/node = %d slot capacity)\n" \
    "$SANDBOX_NODE_COUNT" "$SANDBOX_MACHINE" "$MAX_PODS_PER_NODE" \
    "$((SANDBOX_NODE_COUNT * MAX_PODS_PER_NODE))"
  printf "  pod CIDR:   %s per cluster (VPC default network capacity: /9 = 8M IPs)\n" "$CLUSTER_IPV4_CIDR"
  printf "  image streaming: %s (on-node cache accelerates image pulls)\n\n" "$ENABLE_IMAGE_STREAMING"

  local pids=() c
  for c in $(cluster_names); do
    create_one "$c" &
    pids+=($!)
  done

  local rc=0
  for pid in "${pids[@]}"; do wait "$pid" || rc=$?; done

  if [[ $rc -ne 0 ]]; then
    err "one or more create jobs failed (rc=$rc). Check /tmp/create-*.log and /tmp/pool-*.log"
    exit $rc
  fi

  section "Post-create — REQUIRED patches for controller / template scheduling"
  cat <<EOF
The agent-sandbox-controller from the v0.5.1 release manifest doesn't know about
the controller-pool taint. You MUST patch it after applying the release manifest
or the controller pod will land on the default pool (contention → the exact
apiserver-saturation issue we hit at 100 QPS).

Apply on EACH cluster after 'kubectl apply -f .../v0.5.1/manifest.yaml':

  kubectl -n agent-sandbox-system patch deployment agent-sandbox-controller \\
    --type=strategic -p '{
      "spec": {"template": {"spec": {
        "nodeSelector": {"cloud.google.com/gke-nodepool": "controller-pool"},
        "tolerations": [{"key": "controller-pool", "operator": "Equal", "value": "true", "effect": "NoSchedule"}]
      }}}
    }'

Same for the fleet-member Deployment (already handled by
deploy/fleet-member-deployment-wi.yaml if you keep it in sync — see the
node-scheduling block at the bottom of that file).

Also verify SandboxTemplates have nodeSelector: cloud.google.com/gke-nodepool=sandbox-pool
so pods land on the workhorse pool, not the (small, tainted) controller pool.
EOF
}

do_scale() {
  local target_count="$1"
  section "Resizing sandbox pool to $target_count nodes on all clusters (parallel)"
  local pids=() c
  for c in $(cluster_names); do
    if ! cluster_exists "$c"; then note "cluster $c doesn't exist — skipping"; continue; fi
    (
      gcloud container clusters resize "$c" \
        --node-pool="$SANDBOX_POOL_NAME" \
        --num-nodes="$target_count" \
        --zone="$ZONE" --project="$PROJECT" --quiet \
        >"/tmp/scale-${c}.log" 2>&1 \
        && ok "$c sandbox pool → $target_count" \
        || err "$c scale failed; see /tmp/scale-${c}.log"
    ) &
    pids+=($!)
  done
  for pid in "${pids[@]}"; do wait "$pid" || true; done
}

do_delete() {
  section "DELETING $N_CLUSTERS clusters (irreversible)"
  read -rp "Type 'yes' to confirm: " confirm
  [[ "$confirm" == "yes" ]] || { note "aborted"; exit 0; }
  local pids=() c
  for c in $(cluster_names); do
    if ! cluster_exists "$c"; then note "cluster $c doesn't exist — skipping"; continue; fi
    (
      gcloud container clusters delete "$c" \
        --zone="$ZONE" --project="$PROJECT" --quiet \
        >"/tmp/delete-${c}.log" 2>&1 \
        && ok "deleted $c" \
        || err "$c delete failed"
    ) &
    pids+=($!)
  done
  for pid in "${pids[@]}"; do wait "$pid" || true; done
}

do_status() {
  section "Cluster status (profile=$PROFILE)"
  printf "%-20s %-12s %-25s %s\n" "CLUSTER" "STATE" "CONTROLLER-POOL" "SANDBOX-POOL"
  for c in $(cluster_names); do
    if cluster_exists "$c"; then
      local state
      state=$(gcloud container clusters describe "$c" --zone="$ZONE" --project="$PROJECT" --format="value(status)" 2>/dev/null)
      local ctrl_size="-" sb_size="-"
      pool_exists "$c" "$CONTROLLER_POOL_NAME" && \
        ctrl_size=$(gcloud container node-pools describe "$CONTROLLER_POOL_NAME" --cluster="$c" --zone="$ZONE" --project="$PROJECT" --format="value(initialNodeCount)" 2>/dev/null)
      pool_exists "$c" "$SANDBOX_POOL_NAME" && \
        sb_size=$(gcloud container node-pools describe "$SANDBOX_POOL_NAME" --cluster="$c" --zone="$ZONE" --project="$PROJECT" --format="value(initialNodeCount)" 2>/dev/null)
      printf "%-20s %-12s %-25s %s nodes\n" "$c" "$state" "$ctrl_size nodes" "$sb_size"
    else
      printf "%-20s %-12s %-25s %s\n" "$c" "MISSING" "-" "-"
    fi
  done
}

# --------------------------------------------------------------------------- #
# Dispatch
# --------------------------------------------------------------------------- #

case "$MODE" in
  create)     do_create ;;
  scale-down) do_scale 0 ;;
  scale-up)   do_scale "$SANDBOX_NODE_COUNT" ;;
  delete)     do_delete ;;
  status)     do_status ;;
  *) err "unknown mode: $MODE (create|scale-down|scale-up|delete|status)"; exit 2 ;;
esac
