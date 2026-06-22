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
# Pre-pull SWE-bench task images onto every node, so warm pools become
# claimable without paying the multi-GB image pull at warm-up time.
#
# How: a DaemonSet with one *init container per unique image* (no-op command,
# IfNotPresent) forces every node's kubelet to pull + cache each image; a tiny
# pause main container keeps the DS scheduled. Runs in parallel across nodes and
# automatically covers new (autoscaled) nodes. Since the task set is known up
# front, we know exactly which images to pre-pull.
#
# Usage:
#   ./prepull.sh -n 4                 # pre-pull first 4 dataset images
#   ./prepull.sh -n 4 --offset 2      # skip the first 2 (e.g. already cached)
#   ./prepull.sh img1 img2 ...        # pre-pull explicit images
#   ./prepull.sh --status             # show DaemonSet readiness
#   ./prepull.sh --delete             # remove the DaemonSet (cached images stay)
#
# Flags: -n|--tasks N  --offset N  --delete  --status  -h|--help
# Env:   NAMESPACE, DATASET, DATASET_SPLIT, NODE_SELECTOR_KEY/VAL,
#        IMAGE_PULL_SECRET, DS_NAME, PAUSE_IMAGE, READY_TIMEOUT

set -euo pipefail

NAMESPACE="${NAMESPACE:-rl-tunix-swebench}"
DATASET="${DATASET:-R2E-Gym/SWE-Bench-Verified}"
DATASET_SPLIT="${DATASET_SPLIT:-test}"
TASKS="${TASKS:-}"
OFFSET="${OFFSET:-0}"
NODE_SELECTOR_KEY="${NODE_SELECTOR_KEY:-}"
NODE_SELECTOR_VAL="${NODE_SELECTOR_VAL:-}"
IMAGE_PULL_SECRET="${IMAGE_PULL_SECRET:-}"
DS_NAME="${DS_NAME:-rl-tunix-prepull}"
PAUSE_IMAGE="${PAUSE_IMAGE:-registry.k8s.io/pause:3.10}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
LABEL_KEY="app"; LABEL_VAL="rl-tunix-e2e"; LABEL="${LABEL_KEY}=${LABEL_VAL}"

ACTION="apply"
ARG_IMAGES=()
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--tasks) TASKS="$2"; shift 2 ;;
    --offset) OFFSET="$2"; shift 2 ;;
    --delete) ACTION="delete"; shift ;;
    --status) ACTION="status"; shift ;;
    -h|--help) sed -n '16,33p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "Unknown flag: $1" >&2; exit 2 ;;
    *) ARG_IMAGES+=("$1"); shift ;;
  esac
done

if [ -t 1 ]; then C_B=$'\033[1m'; C_G=$'\033[32m'; C_0=$'\033[0m'; else C_B=""; C_G=""; C_0=""; fi
info() { printf '%s▶%s %s\n' "$C_B" "$C_0" "$*"; }
ok()   { printf '  %s✓%s %s\n' "$C_G" "$C_0" "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
now_ms() { python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || echo $(( $(date +%s) * 1000 )); }
fmt_s()  { awk "BEGIN{printf \"%.1f\", $1/1000}"; }
kc() { kubectl -n "$NAMESPACE" "$@"; }

command -v kubectl >/dev/null || die "kubectl not found"

if [ "$ACTION" = "delete" ]; then
  kc delete daemonset "$DS_NAME" --ignore-not-found
  exit 0
fi
if [ "$ACTION" = "status" ]; then
  kc get daemonset "$DS_NAME" -o wide 2>/dev/null || die "DaemonSet $DS_NAME not found"
  exit 0
fi

# ---- resolve image list ---------------------------------------------------- #
IMAGES=()
if [ "${#ARG_IMAGES[@]}" -gt 0 ]; then
  IMAGES=("${ARG_IMAGES[@]}")
else
  command -v curl >/dev/null || die "curl not found"
  command -v jq >/dev/null || die "jq not found"
  [ -n "$TASKS" ] || die "specify -n <tasks> or pass explicit image args"
  ds_enc=$(printf '%s' "$DATASET" | sed 's;/;%2F;g')
  url="https://datasets-server.huggingface.co/rows?dataset=${ds_enc}&config=default&split=${DATASET_SPLIT}&offset=${OFFSET}&length=${TASKS}"
  while IFS= read -r line; do [ -n "$line" ] && IMAGES+=("$line"); done < <(
    curl -s "$url" | jq -r '.rows[].row.docker_image'
  )
fi
# de-dupe, preserve order
UNIQ=()
for img in "${IMAGES[@]}"; do
  dup=0; for u in "${UNIQ[@]:-}"; do [ "$u" = "$img" ] && dup=1 && break; done
  [ "$dup" = "0" ] && UNIQ+=("$img")
done
[ "${#UNIQ[@]}" -gt 0 ] || die "no images to pre-pull"

info "pre-pulling ${#UNIQ[@]} unique image(s) onto all nodes via DaemonSet ${DS_NAME}"
i=1; for img in "${UNIQ[@]}"; do echo "      ${i}. ${img}"; i=$(( i + 1 )); done

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE" >/dev/null

# ---- render DaemonSet ------------------------------------------------------ #
render() {
  cat <<YAML
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${DS_NAME}
  namespace: ${NAMESPACE}
  labels:
    ${LABEL_KEY}: ${LABEL_VAL}
spec:
  selector:
    matchLabels:
      ${LABEL_KEY}: ${LABEL_VAL}
      role: prepull
  template:
    metadata:
      labels:
        ${LABEL_KEY}: ${LABEL_VAL}
        role: prepull
    spec:
YAML
  if [ -n "$NODE_SELECTOR_KEY" ] && [ -n "$NODE_SELECTOR_VAL" ]; then
    echo "      nodeSelector:"
    echo "        ${NODE_SELECTOR_KEY}: ${NODE_SELECTOR_VAL}"
  fi
  [ -n "$IMAGE_PULL_SECRET" ] && { echo "      imagePullSecrets:"; echo "      - name: ${IMAGE_PULL_SECRET}"; }
  echo "      terminationGracePeriodSeconds: 0"
  echo "      initContainers:"
  local idx=0
  for img in "${UNIQ[@]}"; do
    cat <<YAML
      - name: pull-${idx}
        image: ${img}
        imagePullPolicy: IfNotPresent
        command: ["sh", "-c", "exit 0"]
        resources:
          requests:
            cpu: "10m"
            memory: "16Mi"
YAML
    idx=$(( idx + 1 ))
  done
  cat <<YAML
      containers:
      - name: pause
        image: ${PAUSE_IMAGE}
        imagePullPolicy: IfNotPresent
        resources:
          requests:
            cpu: "10m"
            memory: "16Mi"
YAML
}

render | kubectl apply -f - >/dev/null
ok "DaemonSet applied"

# ---- wait for every node to finish pulling --------------------------------- #
info "waiting for all nodes to cache the images (init containers run the pulls)…"
t0=$(now_ms)
deadline=$(( $(date +%s) + READY_TIMEOUT ))
while :; do
  desired=$(kc get daemonset "$DS_NAME" -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || echo 0)
  ready=$(kc get daemonset "$DS_NAME" -o jsonpath='{.status.numberReady}' 2>/dev/null || echo 0)
  desired=${desired:-0}; ready=${ready:-0}
  printf '\r  nodes ready: %s/%s ' "$ready" "$desired"
  if [ "$desired" -gt 0 ] && [ "$ready" -ge "$desired" ]; then echo; break; fi
  [ "$(date +%s)" -ge "$deadline" ] && { echo; die "pre-pull not complete ($ready/$desired) within ${READY_TIMEOUT}s"; }
  sleep 3
done
elapsed=$(( $(now_ms) - t0 ))

ok "pre-pull complete on all ${ready} node(s) in $(fmt_s $elapsed)s"
info "images are now node-cached; warm pools for these images will skip the pull."
echo "  (run './prepull.sh --delete' to remove the DaemonSet; cached images persist)"
