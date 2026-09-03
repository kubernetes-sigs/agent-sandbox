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

# End-to-end check of the golden-snapshot warm pool on a GKE cluster that
# has Pod Snapshots enabled and the agent-sandbox controller installed.
#
# Required env:
#   SNAPSHOT_BUCKET  GCS bucket for snapshots (IAM already granted, README §2)
#   DEMO_IMAGE       pushed image built from image/ (README §3)
set -euo pipefail
cd "$(dirname "$0")"

: "${SNAPSHOT_BUCKET:?set SNAPSHOT_BUCKET}"
: "${DEMO_IMAGE:?set DEMO_IMAGE}"

state() { # state <pod> -> prints the /state JSON
    kubectl exec "$1" -c probe -- python -c \
        'import urllib.request;print(urllib.request.urlopen("http://localhost:8888/state").read().decode())'
}
post() { # post <pod> <path> [body]
    kubectl exec "$1" -c probe -- python -c \
        "import urllib.request;print(urllib.request.urlopen(urllib.request.Request(\"http://localhost:8888$2\",data=b\"${3:-}\",method=\"POST\")).read().decode())"
}
field() { # field <json> <key>
    python3 -c "import json,sys;print(json.loads(sys.argv[1]).get(sys.argv[2]))" "$1" "$2"
}
pod_of_claim() { kubectl get sandboxclaim "$1" -o jsonpath='{.status.sandbox.name}'; }

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

echo "=== 1. Apply snapshot config, template, pool"
export SNAPSHOT_BUCKET DEMO_IMAGE
# fixed-name triggers from a previous run would satisfy the waits below
kubectl delete podsnapshotmanualtriggers golden-v1 golden-v2 golden-v3 --ignore-not-found
envsubst < 00-storageconfig.yaml | kubectl apply -f -
kubectl apply -f 05-podsnapshotpolicy.yaml
envsubst < 10-sandboxtemplate.yaml | kubectl apply -f -
kubectl apply -f 20-sandboxwarmpool.yaml
# wait on the pool's own status: a label-selector wait races the controller
# creating the first Sandbox objects ("no matching resources found")
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=2 --timeout=600s

echo "=== 2. Primer: claim, seed memory + filesystem state"
kubectl apply -f 25-primer-claim.yaml
kubectl wait --for=condition=Ready sandboxclaim/podsnap-primer --timeout=300s
PRIMER_POD=$(pod_of_claim podsnap-primer)
post "$PRIMER_POD" /bump; post "$PRIMER_POD" /bump; post "$PRIMER_POD" /bump
post "$PRIMER_POD" /write "golden-v1"
PRIMER_STATE=$(state "$PRIMER_POD")
PRIMER_NONCE=$(field "$PRIMER_STATE" boot_nonce)
echo "primer: $PRIMER_STATE"
[ "$(field "$PRIMER_STATE" counter)" = "3" ]

echo "=== 3. Take the golden snapshot"
export PRIMER_POD TRIGGER_SUFFIX=v1
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
kubectl wait --for=condition=Triggered podsnapshotmanualtrigger/golden-v1 --timeout=600s
SNAP_V1=$(snap_of_trigger golden-v1)
kubectl wait --for=condition=Ready "podsnapshots.podsnapshot.gke.io/$SNAP_V1" --timeout=600s
echo "golden snapshot: $SNAP_V1"

echo "=== 4. Roll the pool: fresh members must boot restored"
kubectl delete sandbox -l agents.x-k8s.io/warm-pool-sandbox --wait=true
sleep 10   # let the pool status observe the deletions before waiting on it
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=2 --timeout=600s
for POD in $(kubectl get pods -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{.items[*].metadata.name}'); do
    S=$(state "$POD")
    echo "pool member $POD: $S"
    [ "$(field "$S" boot_nonce)" = "$PRIMER_NONCE" ]   # memory restored
    [ "$(field "$S" counter)" = "3" ]                  # memory restored
    [ "$(field "$S" marker)" = "golden-v1" ]           # filesystem restored
done

echo "=== 5. Workers adopt pre-warmed restored members"
POOL_BEFORE=$(kubectl get sandbox -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{.items[*].metadata.name}')
kubectl apply -f 40-worker-claims.yaml
kubectl wait --for=condition=Ready sandboxclaim/podsnap-worker-1 sandboxclaim/podsnap-worker-2 --timeout=300s
for CLAIM in podsnap-worker-1 podsnap-worker-2; do
    POD=$(pod_of_claim "$CLAIM")
    S=$(state "$POD")
    echo "$CLAIM ($POD): $S"
    case " $POOL_BEFORE " in *" $POD "*) ;; *) echo "$CLAIM did not adopt a pool member"; exit 1;; esac
    [ "$(field "$S" boot_nonce)" = "$PRIMER_NONCE" ]
    [ "$(field "$S" marker)" = "golden-v1" ]
done

echo "=== 6. Subsequent snapshot: latest wins for NEW pods only"
post "$PRIMER_POD" /bump                       # counter 3 -> 4
post "$PRIMER_POD" /write "golden-v2"
export TRIGGER_SUFFIX=v2
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
kubectl wait --for=condition=Triggered podsnapshotmanualtrigger/golden-v2 --timeout=600s
SNAP_V2=$(snap_of_trigger golden-v2)
kubectl wait --for=condition=Ready "podsnapshots.podsnapshot.gke.io/$SNAP_V2" --timeout=600s
# members pre-warmed before v2 still carry v1 (mixed generations)...
STALE=$(kubectl get pods -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{.items[0].metadata.name}')
[ "$(field "$(state "$STALE")" marker)" = "golden-v1" ]
echo "pre-v2 member $STALE still golden-v1 (expected)"
# ...until the pool is rolled again
kubectl delete sandbox -l agents.x-k8s.io/warm-pool-sandbox --wait=true
sleep 10
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=2 --timeout=600s
FRESH=$(kubectl get pods -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{.items[0].metadata.name}')
S=$(state "$FRESH")
echo "post-v2 member $FRESH: $S"
[ "$(field "$S" marker)" = "golden-v2" ]
[ "$(field "$S" counter)" = "4" ]

echo "=== 7. Extension: snapshot-then-roll keeps a size-1 pool on the latest state"
kubectl patch sandboxwarmpool podsnap-golden-pool --type merge -p '{"spec":{"replicas":1}}'
post "$PRIMER_POD" /bump                       # counter 4 -> 5
post "$PRIMER_POD" /write "golden-v3"
export TRIGGER_SUFFIX=v3
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
kubectl wait --for=condition=Triggered podsnapshotmanualtrigger/golden-v3 --timeout=600s
SNAP_V3=$(snap_of_trigger golden-v3)
# THE GATE: roll only after the PodSnapshot is Ready — rolling on trigger
# completion alone re-warms from the previous snapshot.
kubectl wait --for=condition=Ready "podsnapshots.podsnapshot.gke.io/$SNAP_V3" --timeout=600s
kubectl delete sandbox -l agents.x-k8s.io/warm-pool-sandbox --wait=true
sleep 10
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=1 --timeout=600s
POOL_BEFORE=$(kubectl get sandbox -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{.items[*].metadata.name}')
kubectl apply -f 50-refresh-claim.yaml
kubectl wait --for=condition=Ready sandboxclaim/podsnap-worker-3 --timeout=300s
POD=$(pod_of_claim podsnap-worker-3)
S=$(state "$POD")
echo "podsnap-worker-3 ($POD): $S"
case " $POOL_BEFORE " in *" $POD "*) ;; *) echo "podsnap-worker-3 did not adopt a pool member"; exit 1;; esac
[ "$(field "$S" boot_nonce)" = "$PRIMER_NONCE" ]
[ "$(field "$S" counter)" = "5" ]
[ "$(field "$S" marker)" = "golden-v3" ]

echo "=== 8. Snapshot deletion works (validates the service agent's bucket IAM)"
kubectl delete "podsnapshots.podsnapshot.gke.io/$SNAP_V1" --timeout=300s

echo "ALL CHECKS PASSED"
