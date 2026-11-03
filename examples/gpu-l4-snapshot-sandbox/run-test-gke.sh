#!/bin/bash
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

# End-to-end check of snapshot, suspend, and restore for an L4 GPU sandbox on
# a GKE cluster with Pod Snapshots enabled, a gVisor L4 node pool, and the
# agent-sandbox controller installed.
#
# Required env:
#   SNAPSHOT_BUCKET  GCS bucket for snapshots (IAM already granted, README §2)
#   DEMO_IMAGE       pushed image built from image/ (README §3)
set -euo pipefail
cd "$(dirname "$0")"

: "${SNAPSHOT_BUCKET:?set SNAPSHOT_BUCKET}"
: "${DEMO_IMAGE:?set DEMO_IMAGE}"
SB=gpu-l4-sandbox

# The namespaced objects are in default (the PodSnapshotStorageConfig is
# cluster-scoped). Pin each kubectl call to default, so that the script
# works with any current namespace.
kubectl() { command kubectl --namespace=default "$@"; }

call() { # call <GET|POST> <path> -> prints the JSON reply
    kubectl exec "$SB" -c probe -- python -c \
        "import urllib.request as u;print(u.urlopen(u.Request('http://localhost:8888$2',data=b'' if '$1'=='POST' else None,method='$1')).read().decode())"
}
field() { # field <json> <key>
    python3 -c "import json,sys;print(json.loads(sys.argv[1]).get(sys.argv[2]))" "$1" "$2"
}
snap_of_trigger() { # snap_of_trigger <trigger> -> PodSnapshot name (retries until set)
    for _ in $(seq 1 120); do
        NAME=$(kubectl get podsnapshotmanualtrigger "$1" -o json | python3 -c '
import json,sys
sc=json.load(sys.stdin).get("status",{}).get("snapshotCreated")
print(sc.get("name","") if isinstance(sc,dict) else (sc or ""))')
        [ -n "$NAME" ] && { echo "$NAME"; return 0; }
        sleep 5
    done
    echo "trigger $1 never reported a created snapshot" >&2; return 1
}

echo "=== 1. Apply snapshot config and the GPU Sandbox"
export SNAPSHOT_BUCKET DEMO_IMAGE TRIGGER_SUFFIX="s$(date +%s)"
envsubst < 00-storageconfig.yaml | kubectl apply -f -
kubectl apply -f 10-podsnapshotpolicy.yaml
envsubst < 20-sandbox.yaml | kubectl apply -f -
kubectl wait sandbox/$SB --for=condition=Ready --timeout=900s

echo "=== 2. Seed GPU state"
# A rerun can find the Sandbox from a previous run, or a pod restored from
# its snapshot, so assert relative to the counter at the start.
START=$(field "$(call GET /state)" counter)
for _ in 1 2 3; do call POST /bump >/dev/null; done
BEFORE=$(call GET /state)
echo "before: $BEFORE"
[ "$(field "$BEFORE" counter)" = $((START + 3)) ] || { echo "FAIL: counter is not $((START + 3))"; exit 1; }

echo "=== 3. Snapshot"
T0=$(date +%s)
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
kubectl wait --for=condition=Triggered "podsnapshotmanualtrigger/$SB-$TRIGGER_SUFFIX" --timeout=900s
SNAP=$(snap_of_trigger "$SB-$TRIGGER_SUFFIX")
kubectl wait --for=condition=Ready "podsnapshots.podsnapshot.gke.io/$SNAP" --timeout=900s
echo "snapshot $SNAP Ready in $(( $(date +%s) - T0 ))s"

echo "=== 4. Suspend (releases the L4)"
kubectl patch sandbox $SB --type merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl wait --for=delete pod/$SB --timeout=300s

echo "=== 5. Resume from the snapshot"
T0=$(date +%s)
kubectl patch sandbox $SB --type merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl wait sandbox/$SB --for=condition=Ready --timeout=900s
echo "resumed in $(( $(date +%s) - T0 ))s"
AFTER=$(call GET /state)
echo "after:  $AFTER"
for KEY in boot_nonce counter checksum; do
    [ "$(field "$BEFORE" $KEY)" = "$(field "$AFTER" $KEY)" ] || {
        echo "FAIL: $KEY changed across restore (cold start?)"; exit 1; }
done

echo "=== 6. GPU still usable after restore"
BUMPED=$(call POST /bump)
[ "$(field "$BUMPED" counter)" = $((START + 4)) ] || { echo "FAIL: counter is not $((START + 4)) after bump"; exit 1; }

echo "PASS: the L4 sandbox restored its process and GPU memory from $SNAP"
