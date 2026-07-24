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
#
# End-to-end test for running SWE-bench tasks on Agent Sandbox warm pools.
#
# Walks the full flow with one of three warm-pool strategies, times every
# phase, prints all created objects (SandboxTemplate / SandboxWarmPool /
# SandboxClaim / Sandbox / Pod), and prints a benchmark breakdown with the
# total end-to-end wall-clock at the end. Uses only kubectl + curl + jq
# (no Python / SDK). Real R2E-Gym/SWE-Bench-Verified images from Docker Hub.
#
# Usage:
#   ./e2e_test.sh                        # interactive menu
#   ./e2e_test.sh -s naive -n 2 -y       # non-interactive
#   STRATEGY=sliding TASKS=3 WINDOW_SIZE=1 ./e2e_test.sh -y
#
# Flags:  -s|--strategy none|naive|sliding   -n|--tasks N
#         -w|--window N   -y|--yes   --no-cleanup   -h|--help

set -euo pipefail

# --------------------------------------------------------------------------- #
# Configuration (env overridable; flags below take precedence)
# --------------------------------------------------------------------------- #
STRATEGY="${STRATEGY:-}"
TASKS="${TASKS:-}"
WINDOW_SIZE="${WINDOW_SIZE:-2}"
MAX_WARMPOOL_SIZE="${MAX_WARMPOOL_SIZE:-8}"
MAX_CONCURRENT="${MAX_CONCURRENT:-1}"   # concurrency: sizes pools AND runs claim+exec in parallel (waves)
NAMESPACE="${NAMESPACE:-rl-tunix-swebench}"
DATASET="${DATASET:-R2E-Gym/SWE-Bench-Verified}"
DATASET_SPLIT="${DATASET_SPLIT:-test}"
OFFSET="${OFFSET:-0}"
NODE_SELECTOR_KEY="${NODE_SELECTOR_KEY:-}"
NODE_SELECTOR_VAL="${NODE_SELECTOR_VAL:-}"
RUNTIME_CLASS="${RUNTIME_CLASS:-}"
IMAGE_PULL_SECRET="${IMAGE_PULL_SECRET:-}"
READY_TIMEOUT="${READY_TIMEOUT:-1200}"
CLEANUP="${CLEANUP:-1}"
ASSUME_YES="${ASSUME_YES:-0}"

LABEL_KEY="app"
LABEL_VAL="rl-tunix-e2e"
LABEL="${LABEL_KEY}=${LABEL_VAL}"
GROUP="extensions.agents.x-k8s.io"
PROBE='echo READY $(hostname); git -C /testbed log -1 --oneline 2>/dev/null || ls -d /testbed 2>/dev/null || ls /'

while [ $# -gt 0 ]; do
  case "$1" in
    -s|--strategy) STRATEGY="$2"; shift 2 ;;
    -n|--tasks) TASKS="$2"; shift 2 ;;
    -w|--window) WINDOW_SIZE="$2"; shift 2 ;;
    -c|--concurrency) MAX_CONCURRENT="$2"; shift 2 ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    --no-cleanup) CLEANUP=0; shift ;;
    -h|--help)
      cat <<'USAGE'
e2e_test.sh — end-to-end Agent Sandbox warm-pool test for SWE-bench tasks

Usage:
  ./e2e_test.sh                      # interactive menu
  ./e2e_test.sh -s naive -n 2 -y     # non-interactive
  STRATEGY=sliding TASKS=3 WINDOW_SIZE=1 ./e2e_test.sh -y

Flags:
  -s, --strategy  none | naive | sliding
  -n, --tasks     number of tasks (dataset rows) to run
  -w, --window    sliding-window size (images kept warm)
  -c, --concurrency  parallel claim+exec (waves); also the pool-sizing budget [1]
  -y, --yes       skip the interactive menu (use env/flags/defaults)
      --no-cleanup leave created objects on the cluster
  -h, --help      show this help

Key env: NAMESPACE, MAX_WARMPOOL_SIZE, NODE_SELECTOR_KEY/VAL, RUNTIME_CLASS,
         IMAGE_PULL_SECRET, READY_TIMEOUT, DATASET, DATASET_SPLIT
Requires: kubectl (cluster with agent-sandbox extensions), curl, jq
USAGE
      exit 0 ;;
    *) echo "Unknown arg: $1" >&2; exit 2 ;;
  esac
done

# --------------------------------------------------------------------------- #
# Colors / output helpers
# --------------------------------------------------------------------------- #
if [ -t 1 ]; then C_B=$'\033[1m'; C_G=$'\033[32m'; C_Y=$'\033[33m'; C_R=$'\033[31m'; C_0=$'\033[0m'
else C_B=""; C_G=""; C_Y=""; C_R=""; C_0=""; fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s▶%s %s\n' "$C_B" "$C_0" "$*"; }
ok()   { printf '  %s✓%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_Y" "$C_0" "$*"; }
die()  { printf '%sERROR:%s %s\n' "$C_R" "$C_0" "$*" >&2; exit 1; }

# --------------------------------------------------------------------------- #
# Timing: ms clock + per-phase accumulators (bash 3.2 friendly)
# --------------------------------------------------------------------------- #
now_ms() { python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || echo $(( $(date +%s) * 1000 )); }
fmt_s()  { awk "BEGIN{printf \"%.2f\", $1/1000}"; }

T_PREFLIGHT=0; T_FETCH=0; T_NAMESPACE=0; T_PROVISION=0
T_WAITREADY=0; T_CLAIM=0; T_EXEC=0; T_TEARDOWN=0
T_TASKS_WALL=0          # wall-clock of the (possibly parallel) claim+exec region
add_ms() { eval "$1=\$(( \${$1:-0} + $2 ))"; }   # $1 phase var, $2 delta ms

SCRIPT_START=$(now_ms)
TASKS_DONE=0
TOTAL_CLAIMS=0          # SandboxClaims started (counted in parent shell)
WARM_REPLICAS_TOTAL=0   # cumulative warm-pool replicas provisioned over the run
ACTIVE_REPLICAS=0       # currently-warm replicas
PEAK_REPLICAS=0         # max concurrent warm replicas (footprint differentiator)
POOL_NAMES=(); POOL_REPS=()   # pool -> reps map (bash 3.2: parallel arrays)
map_set_reps() { POOL_NAMES+=("$1"); POOL_REPS+=("$2"); }
map_get_reps() {
  local i
  for i in "${!POOL_NAMES[@]}"; do
    [ "${POOL_NAMES[$i]}" = "$1" ] && { echo "${POOL_REPS[$i]}"; return; }
  done
  echo 0
}

# --------------------------------------------------------------------------- #
# kubectl convenience
# --------------------------------------------------------------------------- #
kc() { kubectl -n "$NAMESPACE" "$@"; }

# --------------------------------------------------------------------------- #
# Fast API path: one `kubectl proxy` (auth once), then hit the local API over
# curl for the hot-path polls/creates. Avoids the ~1s/call kubectl tax (auth
# plugin + TLS + process spawn) so claim/ready latency reflects the controller,
# not the harness. GETs go through the proxy; creates POST to it.
# --------------------------------------------------------------------------- #
SANDBOX_GROUP="agents.x-k8s.io"               # core Sandbox CRD group
PROXY_PORT="${PROXY_PORT:-8693}"
PROXY_BASE="http://127.0.0.1:${PROXY_PORT}"
PROXY_PID=""
start_proxy() {
  kubectl proxy --port="$PROXY_PORT" >/dev/null 2>&1 &
  PROXY_PID=$!
  local d=$(( $(date +%s) + 15 ))
  while :; do
    curl -s -o /dev/null "${PROXY_BASE}/api" && { ok "kubectl proxy up on :${PROXY_PORT} (fast API path)"; return 0; }
    [ "$(date +%s)" -ge "$d" ] && die "kubectl proxy did not come up on :${PROXY_PORT}"
    sleep 0.2
  done
}
stop_proxy() { [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null; PROXY_PID=""; }
ns_api() { printf '%s/apis/%s/v1beta1/namespaces/%s' "$PROXY_BASE" "$1" "$NAMESPACE"; }  # $1=group

template_name() {
  local img="$1" h
  if command -v md5sum >/dev/null 2>&1; then h=$(printf '%s' "$img" | md5sum | cut -c1-12)
  else h=$(printf '%s' "$img" | md5 -q | cut -c1-12); fi
  echo "r2e-img-$h"
}
pool_name() { echo "pool-$1"; }

# Warm-pool replica sizing (mirrors sizing.py): size each image's pool to its
# share of the concurrency budget, >=1, never more than its task count or cap.
#   compute_replicas <tasks_image> <tasks_total> <max_concurrent> <max_pool>
compute_replicas() {
  awk -v ti="$1" -v tt="$2" -v mc="$3" -v mp="$4" 'BEGIN{
    if (ti<=0){print 0; exit}
    if (tt<=0) tt=ti
    s=mc*ti/tt; r=int(s+0.5); if (r<1) r=1
    if (r>ti) r=ti; if (r>mp) r=mp
    print r
  }'
}

# --------------------------------------------------------------------------- #
# Resource builders
# --------------------------------------------------------------------------- #
apply_template() {  # $1 image  $2 template_name
  local img="$1" tn="$2"
  {
    cat <<YAML
apiVersion: ${GROUP}/v1beta1
kind: SandboxTemplate
metadata:
  name: ${tn}
  namespace: ${NAMESPACE}
  labels:
    ${LABEL_KEY}: ${LABEL_VAL}
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: ${tn}
    spec:
YAML
    [ -n "$RUNTIME_CLASS" ] && echo "      runtimeClassName: ${RUNTIME_CLASS}"
    if [ -n "$NODE_SELECTOR_KEY" ] && [ -n "$NODE_SELECTOR_VAL" ]; then
      echo "      nodeSelector:"
      echo "        ${NODE_SELECTOR_KEY}: ${NODE_SELECTOR_VAL}"
    fi
    [ -n "$IMAGE_PULL_SECRET" ] && { echo "      imagePullSecrets:"; echo "      - name: ${IMAGE_PULL_SECRET}"; }
    cat <<YAML
      containers:
      - name: agent-runtime
        image: ${img}
        command: ["sleep", "infinity"]
        stdin: true
        tty: true
        resources:
          requests:
            cpu: "250m"
            memory: "512Mi"
YAML
  } | kubectl apply -f - >/dev/null
}

apply_warmpool() {  # $1 pool  $2 template  $3 replicas
  kubectl apply -f - >/dev/null <<YAML
apiVersion: ${GROUP}/v1beta1
kind: SandboxWarmPool
metadata:
  name: $1
  namespace: ${NAMESPACE}
  labels:
    ${LABEL_KEY}: ${LABEL_VAL}
spec:
  replicas: $3
  sandboxTemplateRef:
    name: $2
YAML
}

wait_pool_ready() {  # $1 pool  $2 expected
  local pool="$1" want="$2" ready deadline
  deadline=$(( $(date +%s) + READY_TIMEOUT ))
  while :; do
    ready=$(curl -s "$(ns_api "$GROUP")/sandboxwarmpools/${pool}" | jq -r '.status.readyReplicas // 0' 2>/dev/null)
    ready=${ready:-0}
    [ "$ready" -ge "$want" ] 2>/dev/null && { ok "pool $pool ready ($ready/$want)"; return 0; }
    [ "$(date +%s)" -ge "$deadline" ] && die "pool $pool not ready ($ready/$want) within ${READY_TIMEOUT}s"
    sleep 0.2
  done
}

CLAIM_SEQ=0
claim_sandbox() {  # $1 pool  -> echoes claim name (POST via proxy, fast)
  CLAIM_SEQ=$(( CLAIM_SEQ + 1 ))
  local claim="e2e-claim-${CLAIM_SEQ}-$RANDOM" pool="$1" code body
  body="{\"apiVersion\":\"${GROUP}/v1beta1\",\"kind\":\"SandboxClaim\",\"metadata\":{\"name\":\"${claim}\",\"namespace\":\"${NAMESPACE}\",\"labels\":{\"${LABEL_KEY}\":\"${LABEL_VAL}\"}},\"spec\":{\"warmPoolRef\":{\"name\":\"${pool}\"}}}"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data "$body" "$(ns_api "$GROUP")/sandboxclaims")
  [ "$code" = "201" ] || die "claim POST failed (HTTP $code)"
  echo "$claim"
}

resolve_sandbox() {  # $1 claim -> echoes sandbox name
  local claim="$1" sb deadline
  deadline=$(( $(date +%s) + READY_TIMEOUT ))
  while :; do
    sb=$(curl -s "$(ns_api "$GROUP")/sandboxclaims/${claim}" | jq -r '.status.sandbox.name // empty' 2>/dev/null)
    [ -n "$sb" ] && { echo "$sb"; return 0; }
    [ "$(date +%s)" -ge "$deadline" ] && die "claim $claim did not resolve a sandbox"
    sleep 0.1
  done
}

wait_sandbox_ready() {  # $1 sandbox
  local sb="$1" st deadline
  deadline=$(( $(date +%s) + READY_TIMEOUT ))
  while :; do
    st=$(curl -s "$(ns_api "$SANDBOX_GROUP")/sandboxes/${sb}" \
         | jq -r '(.status.conditions[]? | select(.type=="Ready") | .status) // empty' 2>/dev/null)
    [ "$st" = "True" ] && return 0
    [ "$(date +%s)" -ge "$deadline" ] && die "sandbox $sb not Ready within ${READY_TIMEOUT}s"
    sleep 0.1
  done
}

pod_of() {  # $1 sandbox -> echoes pod name
  local sb="$1" pod
  pod=$(curl -s "$(ns_api "$SANDBOX_GROUP")/sandboxes/${sb}" \
        | jq -r '.metadata.annotations["agents.x-k8s.io/pod-name"] // empty' 2>/dev/null)
  echo "${pod:-$sb}"
}

# Claim a sandbox from $1 and exec the probe; accumulates CLAIM/EXEC phases.
# One task: claim a sandbox from $1, exec the probe. Runs in a background
# subshell, so it writes results/timings to files in $3 (subshell vars don't
# propagate to the parent); the parent aggregates in run_units().
run_task_unit() {  # $1 pool  $2 label  $3 workdir
  local pool="$1" label="$2" wd="$3" t0 claim sb pod out cms ems
  t0=$(now_ms)
  claim=$(claim_sandbox "$pool")
  sb=$(resolve_sandbox "$claim")
  wait_sandbox_ready "$sb"
  cms=$(( $(now_ms) - t0 ))
  t0=$(now_ms)
  pod=$(pod_of "$sb")
  out=$(kc exec "$pod" -- bash -lc "$PROBE" 2>/dev/null | tr '\n' ' ' || echo "<exec failed>")
  ems=$(( $(now_ms) - t0 ))
  printf '%s %s\n' "$cms" "$ems" > "$wd/ms.$label"
  printf '%s\t%s\t%s\t%s\t%s\n' "$label" "$claim" "$sb" "$pod" "$out" > "$wd/res.$label"
}

# Run a list of "pool|label" task units, up to MAX_CONCURRENT at a time
# (wave-based; bash 3.2 has no `wait -n`). Times the whole region as one
# wall-clock bucket (T_TASKS_WALL) and aggregates per-task claim/exec sums.
run_units() {  # "$@" = pool|label ...
  [ "$#" -gt 0 ] || return 0
  local wd u pool label rc=0 f cms ems lbl claim sb pod out exp got
  local pids; pids=()
  wd=$(mktemp -d "${TMPDIR:-/tmp}/e2e-units.XXXXXX")
  exp=$#
  local t0; t0=$(now_ms)
  for u in "$@"; do
    pool="${u%%|*}"; label="${u##*|}"
    run_task_unit "$pool" "$label" "$wd" &
    pids+=("$!")
    if [ "${#pids[@]}" -ge "$MAX_CONCURRENT" ]; then wait "${pids[@]}" 2>/dev/null || true; pids=(); fi
  done
  [ "${#pids[@]}" -gt 0 ] && { wait "${pids[@]}" 2>/dev/null || true; }
  add_ms T_TASKS_WALL $(( $(now_ms) - t0 ))

  got=0
  for f in "$wd"/ms.*; do
    [ -e "$f" ] || continue
    read -r cms ems < "$f"
    add_ms T_CLAIM "$cms"; add_ms T_EXEC "$ems"
    TOTAL_CLAIMS=$(( TOTAL_CLAIMS + 1 )); TASKS_DONE=$(( TASKS_DONE + 1 )); got=$(( got + 1 ))
  done
  for f in "$wd"/res.*; do
    [ -e "$f" ] || continue
    IFS=$'\t' read -r lbl claim sb pod out < "$f"
    ok "task ${lbl}: claim=${claim} sandbox=${sb} pod=${pod}"
    say "      ↳ ${out}"
  done
  [ "$got" -lt "$exp" ] && warn "$(( exp - got ))/${exp} task(s) failed (no result written)"
  rm -rf "$wd"
}

provision_pool() {  # $1 image  $2 replicas  (side effect only; accumulates PROVISION + WAITREADY)
  local img="$1" reps="$2" tn pool t0
  tn=$(template_name "$img"); pool=$(pool_name "$tn")
  t0=$(now_ms); apply_template "$img" "$tn"; apply_warmpool "$pool" "$tn" "$reps"; add_ms T_PROVISION $(( $(now_ms) - t0 ))
  t0=$(now_ms); wait_pool_ready "$pool" "$reps"; add_ms T_WAITREADY $(( $(now_ms) - t0 ))
  map_set_reps "$pool" "$reps"
  WARM_REPLICAS_TOTAL=$(( WARM_REPLICAS_TOTAL + reps ))
  ACTIVE_REPLICAS=$(( ACTIVE_REPLICAS + reps ))
  if [ "$ACTIVE_REPLICAS" -gt "$PEAK_REPLICAS" ]; then PEAK_REPLICAS=$ACTIVE_REPLICAS; fi
}

teardown_pool() {  # $1 image
  local img="$1" tn pool t0
  tn=$(template_name "$img"); pool=$(pool_name "$tn")
  t0=$(now_ms)
  kc delete sandboxwarmpool "$pool" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kc delete sandboxtemplate "$tn" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  add_ms T_TEARDOWN $(( $(now_ms) - t0 ))
  ACTIVE_REPLICAS=$(( ACTIVE_REPLICAS - $(map_get_reps "$pool") ))
  if [ "$ACTIVE_REPLICAS" -lt 0 ]; then ACTIVE_REPLICAS=0; fi
}

# --------------------------------------------------------------------------- #
# Cleanup (label-scoped) — also run from the EXIT/INT trap
# --------------------------------------------------------------------------- #
cleanup_all() {
  kc delete sandboxclaims -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kc delete sandboxwarmpools -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kc delete sandboxtemplates -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
CLEANED=0
on_exit() {
  local rc=$?
  stop_proxy
  if [ "$CLEANUP" = "1" ] && [ "$CLEANED" = "0" ]; then
    echo; warn "cleaning up (trap)…"; cleanup_all
  fi
  exit $rc
}
trap on_exit EXIT INT TERM

# --------------------------------------------------------------------------- #
# Object dump
# --------------------------------------------------------------------------- #
dump_objects() {
  echo
  info "Created objects in namespace ${NAMESPACE}"
  echo "  --- SandboxTemplates / SandboxWarmPools ---"
  kc get sandboxtemplates,sandboxwarmpools -l "$LABEL" -o wide 2>/dev/null | sed 's/^/  /' || true
  echo "  --- SandboxClaims ---"
  kc get sandboxclaims -l "$LABEL" -o wide 2>/dev/null | sed 's/^/  /' || true
  echo "  --- Sandboxes / Pods ---"
  kc get sandboxes,pods -o wide 2>/dev/null | sed 's/^/  /' || true
}

# --------------------------------------------------------------------------- #
# Preflight
# --------------------------------------------------------------------------- #
preflight() {
  local t0; t0=$(now_ms)
  command -v kubectl >/dev/null || die "kubectl not found"
  command -v curl >/dev/null || die "curl not found"
  command -v jq >/dev/null || die "jq not found (brew install jq)"
  kubectl version -o json >/dev/null 2>&1 || die "cannot reach the cluster (check kubeconfig)"
  kubectl get crd sandboxwarmpools.${GROUP} >/dev/null 2>&1 || die "agent-sandbox extensions CRDs not installed"
  add_ms T_PREFLIGHT $(( $(now_ms) - t0 ))
  ok "kubectl, curl, jq present; cluster reachable; CRDs found"
}

# --------------------------------------------------------------------------- #
# Fetch task images from the dataset (HF datasets-server REST API)
# --------------------------------------------------------------------------- #
IMAGES=()
fetch_tasks() {
  local t0 url ds_enc; t0=$(now_ms)
  ds_enc=$(printf '%s' "$DATASET" | sed 's;/;%2F;g')
  url="https://datasets-server.huggingface.co/rows?dataset=${ds_enc}&config=default&split=${DATASET_SPLIT}&offset=${OFFSET}&length=${TASKS}"
  local line
  while IFS= read -r line; do [ -n "$line" ] && IMAGES+=("$line"); done < <(
    curl -s "$url" | jq -r '.rows[].row.docker_image' 2>/dev/null
  )
  add_ms T_FETCH $(( $(now_ms) - t0 ))
  [ "${#IMAGES[@]}" -gt 0 ] || die "could not fetch task images from ${DATASET} (HF API)"
  ok "fetched ${#IMAGES[@]} task image(s):"
  local i=1; for img in "${IMAGES[@]}"; do say "      ${i}. ${img}"; i=$(( i + 1 )); done
}

ensure_namespace() {
  local t0; t0=$(now_ms)
  kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE" >/dev/null
  add_ms T_NAMESPACE $(( $(now_ms) - t0 ))
  ok "namespace ${NAMESPACE} ready"
}

# --------------------------------------------------------------------------- #
# Strategies
# --------------------------------------------------------------------------- #
strategy_none() {
  info "Strategy: none (provision a size-1 pool on demand per task)"
  local i=1 img pool
  for img in "${IMAGES[@]}"; do
    info "task ${i}/${#IMAGES[@]}: ${img}"
    provision_pool "$img" 1
    pool=$(pool_name "$(template_name "$img")")
    run_units "${pool}|${i}"
    dump_objects
    teardown_pool "$img"
    i=$(( i + 1 ))
  done
}

# Sorted unique images + counts (parallel arrays U / CNT).
U=(); CNT=()
build_unique() {
  local line c img
  while read -r c img; do U+=("$img"); CNT+=("$c"); done < <(
    printf '%s\n' "${IMAGES[@]}" | sort | uniq -c | sed 's/^ *//'
  )
}

strategy_naive() {
  info "Strategy: naive (pre-warm every unique image up front)"
  build_unique
  local n=${#U[@]} i img reps
  for (( i=0; i<n; i++ )); do
    img="${U[$i]}"; reps=$(compute_replicas "${CNT[$i]}" "${#IMAGES[@]}" "$MAX_CONCURRENT" "$MAX_WARMPOOL_SIZE")
    info "pre-warming ${img} (replicas=${reps})"
    provision_pool "$img" "$reps"
  done
  local t=1 units; units=()
  for (( i=0; i<n; i++ )); do
    img="${U[$i]}"; local pool; pool=$(pool_name "$(template_name "$img")")
    local j; for (( j=0; j<CNT[$i]; j++ )); do units+=("${pool}|${t}"); t=$(( t + 1 )); done
  done
  run_units "${units[@]}"
  dump_objects
  for (( i=0; i<n; i++ )); do teardown_pool "${U[$i]}"; done
}

strategy_sliding() {
  info "Strategy: sliding (keep ${WINDOW_SIZE} image pool(s) warm, roll forward)"
  build_unique
  local n=${#U[@]} i reps img pool next t=1 dumped=0
  # Pre-warm the first WINDOW_SIZE images.
  local w=$WINDOW_SIZE; [ "$w" -gt "$n" ] && w=$n
  for (( i=0; i<w; i++ )); do
    img="${U[$i]}"; reps=$(compute_replicas "${CNT[$i]}" "${#IMAGES[@]}" "$MAX_CONCURRENT" "$MAX_WARMPOOL_SIZE")
    info "pre-warming window slot $(( i + 1 )): ${img} (replicas=${reps})"
    provision_pool "$img" "$reps"
  done
  next=$w
  # Each group is already warm when we reach it: either in the initial window,
  # or pre-warmed by the slide after processing the group W positions earlier.
  for (( i=0; i<n; i++ )); do
    img="${U[$i]}"
    pool=$(pool_name "$(template_name "$img")")
    local j units; units=()
    for (( j=0; j<CNT[$i]; j++ )); do units+=("${pool}|${t}"); t=$(( t + 1 )); done
    run_units "${units[@]}"
    if [ "$dumped" = "0" ]; then dump_objects; dumped=1; fi   # snapshot mid-run (window active)
    teardown_pool "$img"
    # Slide: pre-warm the next out-of-window image.
    if [ "$next" -lt "$n" ]; then
      local nimg nreps
      nimg="${U[$next]}"; nreps=$(compute_replicas "${CNT[$next]}" "${#IMAGES[@]}" "$MAX_CONCURRENT" "$MAX_WARMPOOL_SIZE")
      info "sliding window → pre-warming ${nimg} (replicas=${nreps})"
      provision_pool "$nimg" "$nreps"
      next=$(( next + 1 ))
    fi
  done
}

# --------------------------------------------------------------------------- #
# Interactive menu
# --------------------------------------------------------------------------- #
menu() {
  echo "${C_B}rl-tunix SWE-bench warm-pool e2e test${C_0}"
  echo "Choose a warm-pool strategy:"
  local opt
  PS3="strategy> "
  select opt in "none (on-demand)" "naive (pre-warm all)" "sliding (windowed)"; do
    case "$REPLY" in
      1) STRATEGY=none; break ;; 2) STRATEGY=naive; break ;; 3) STRATEGY=sliding; break ;;
      *) echo "pick 1-3" ;;
    esac
  done
  printf "How many tasks to run? [2] "; read -r ans; TASKS="${ans:-2}"
  if [ "$STRATEGY" = "sliding" ]; then
    printf "Sliding window size (images kept warm)? [%s] " "$WINDOW_SIZE"; read -r ans; WINDOW_SIZE="${ans:-$WINDOW_SIZE}"
  fi
  printf "Tear everything down at the end? [Y/n] "; read -r ans
  case "${ans:-Y}" in n|N) CLEANUP=0 ;; *) CLEANUP=1 ;; esac
}

# --------------------------------------------------------------------------- #
# Report
# --------------------------------------------------------------------------- #
report() {
  local total=$(( $(now_ms) - SCRIPT_START ))
  echo
  echo "${C_B}── Benchmark (strategy=${STRATEGY}, tasks=${TASKS_DONE}) ───────────────${C_0}"
  printf "  %-22s %8ss\n" "preflight"          "$(fmt_s $T_PREFLIGHT)"
  printf "  %-22s %8ss\n" "fetch tasks"        "$(fmt_s $T_FETCH)"
  printf "  %-22s %8ss\n" "create namespace"   "$(fmt_s $T_NAMESPACE)"
  printf "  %-22s %8ss\n" "provision pools"    "$(fmt_s $T_PROVISION)"
  printf "  %-22s %8ss\n" "wait warm (pull)"   "$(fmt_s $T_WAITREADY)"
  printf "  %-22s %8ss\n" "claim sandboxes (Σ)" "$(fmt_s $T_CLAIM)"
  printf "  %-22s %8ss\n" "exec probes (Σ)"     "$(fmt_s $T_EXEC)"
  printf "  %-22s %8ss\n" "tasks region (wall)" "$(fmt_s $T_TASKS_WALL)"
  printf "  %-22s %8ss\n" "teardown"           "$(fmt_s $T_TEARDOWN)"
  echo   "  ──────────────────────────────────────────"
  printf "  %-22s %8ss\n" "${C_B}TOTAL e2e${C_0}" "$(fmt_s $total)"
  echo   "  ──────────────────────────────────────────"
  printf "  %-22s %8d\n" "concurrency (MAX)"    "$MAX_CONCURRENT"
  printf "  %-22s %8d\n" "SandboxClaims started" "$TOTAL_CLAIMS"
  printf "  %-22s %8d\n" "warm replicas (total)" "$WARM_REPLICAS_TOTAL"
  printf "  %-22s %8d\n" "warm replicas (peak)"  "$PEAK_REPLICAS"
}

# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #
main() {
  if [ "$ASSUME_YES" != "1" ] && { [ -z "$STRATEGY" ] || [ -z "$TASKS" ]; }; then
    menu
  fi
  STRATEGY="${STRATEGY:-naive}"; TASKS="${TASKS:-2}"
  case "$STRATEGY" in none|naive|sliding) ;; *) die "invalid strategy: $STRATEGY" ;; esac
  case "$TASKS" in ''|*[!0-9]*) die "tasks must be a positive integer" ;; esac
  [ "$TASKS" -ge 1 ] || die "tasks must be >= 1"

  echo
  info "config: strategy=${STRATEGY} tasks=${TASKS} concurrency=${MAX_CONCURRENT} window=${WINDOW_SIZE} max_pool=${MAX_WARMPOOL_SIZE} ns=${NAMESPACE} cleanup=${CLEANUP}"
  echo

  preflight
  start_proxy
  fetch_tasks
  ensure_namespace

  case "$STRATEGY" in
    none)    strategy_none ;;
    naive)   strategy_naive ;;
    sliding) strategy_sliding ;;
  esac

  if [ "$CLEANUP" = "1" ]; then
    info "tearing down (label ${LABEL})…"
    local t0; t0=$(now_ms); cleanup_all; add_ms T_TEARDOWN $(( $(now_ms) - t0 )); CLEANED=1
    sleep 2
    local left; left=$(kc get sandboxtemplates,sandboxwarmpools,sandboxclaims -l "$LABEL" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    ok "cleanup done (remaining labeled objects: ${left})"
  else
    warn "skipping cleanup (--no-cleanup); remove later with:"
    say  "      kubectl delete sandboxclaims,sandboxwarmpools,sandboxtemplates -l ${LABEL} -n ${NAMESPACE}"
  fi

  report
}

main "$@"
