#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Raw-wire probe of the member -> hub path, with no Python client in the way.
#
#   ./deploy/hub-probe.sh                       # uses $PROJECT / $CLUSTER_NAME
#   CLUSTER_NAME=cluster-a ./deploy/hub-probe.sh
#
# fleet-hubcheck tells you WHICH STEP broke. This tells you WHERE in the stack,
# which is the question you have left once hubcheck says 401 and both Workload
# Identity and the IAM grant look correct.
#
# It runs curl in a pod using the same ServiceAccount and the same mounted
# kubeconfig as the real member, so the only thing removed from the picture is
# our own client code. That makes the result a fork:
#
#   curl succeeds, hubcheck 401s  ->  our client is not sending the token.
#                                     Look at hubauth.py / publisher.py.
#   curl also 401s                ->  the hub genuinely rejects this identity.
#                                     Look at IAM and at the hub cluster.
#
# SelfSubjectReview is the most useful line in the output: it makes the hub
# state, in its own words, which username it resolved the bearer token to. A
# GKE cluster reports `<gsa-email>`, and anything else (or a 401 here too)
# says the token never authenticated.

set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
CLUSTER_NAME="${CLUSTER_NAME:-cluster-a}"
ZONE="${ZONE:-us-central1-a}"
MEMBER_CTX="${MEMBER_CTX:-gke_${PROJECT}_${ZONE}_${CLUSTER_NAME}}"
MEMBER_NS="${MEMBER_NS:-multi-cluster-fleet}"
HUB_NS="${HUB_NS:-fleet-system}"
KSA="${KSA:-fleet-member}"
IMAGE="${IMAGE:-curlimages/curl:8.8.0}"

log() { printf "\033[1;34m[probe]\033[0m %s\n" "$*" >&2; }

# Alpine/busybox in the curl image: `grep -m1`, `sed`, `base64 -d` all exist.
# Deliberately no yq/jq -- adding a parser would mean a second image and a
# second thing that can be wrong.
read -r -d '' SCRIPT <<PROBE || true
set -u
M=http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default
KC=/etc/fleet-hub/kubeconfig

SRV=\$(grep -m1 'server:' \$KC | sed 's/.*server:[[:space:]]*//' | tr -d '"'"'"'"\r')
CA=\$(grep -m1 'certificate-authority-data:' \$KC | sed 's/.*certificate-authority-data:[[:space:]]*//' | tr -d '"'"'"'"\r')
echo "\$CA" | base64 -d > /tmp/ca.crt
TOK=\$(curl -s -H 'Metadata-Flavor: Google' \$M/token | sed -e 's/.*"access_token":"//' -e 's/".*//')

echo "server       : \$SRV"
echo "ca bytes     : \$(wc -c < /tmp/ca.crt)"
echo "token length : \$(printf %s "\$TOK" | wc -c)"
echo
echo "--- tokeninfo: what Google says this token is -----------------------"
curl -s "https://oauth2.googleapis.com/tokeninfo?access_token=\$TOK" | head -c 700
echo; echo
echo "--- GET /version: is the hub reachable at all (no auth needed) ------"
curl -s -o /tmp/o1 -w 'http %{http_code}\n' --cacert /tmp/ca.crt "\$SRV/version" || true
head -c 300 /tmp/o1
echo; echo
echo "--- SelfSubjectReview: who does the hub think we are ----------------"
curl -s -o /tmp/o3 -w 'http %{http_code}\n' --cacert /tmp/ca.crt \
  -H "Authorization: Bearer \$TOK" -H 'Content-Type: application/json' \
  -X POST -d '{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}' \
  "\$SRV/apis/authentication.k8s.io/v1/selfsubjectreviews" || true
head -c 700 /tmp/o3
echo; echo
echo "--- GET the ClusterProfile: the call hubcheck actually makes --------"
curl -s -o /tmp/o2 -w 'http %{http_code}\n' --cacert /tmp/ca.crt \
  -H "Authorization: Bearer \$TOK" \
  "\$SRV/apis/multicluster.x-k8s.io/v1alpha1/namespaces/${HUB_NS}/clusterprofiles/${CLUSTER_NAME}" || true
head -c 700 /tmp/o2
echo
PROBE

OVERRIDES="$(SCRIPT="$SCRIPT" KSA="$KSA" IMAGE="$IMAGE" python3 - <<'PY'
import json, os
print(json.dumps({"spec": {
    "serviceAccountName": os.environ["KSA"],
    "restartPolicy": "Never",
    "containers": [{
        "name": "hub-probe",
        "image": os.environ["IMAGE"],
        "command": ["sh", "-c", os.environ["SCRIPT"]],
        "volumeMounts": [{"name": "hub", "mountPath": "/etc/fleet-hub",
                          "readOnly": True}],
    }],
    "volumes": [{"name": "hub", "configMap": {"name": "hub-kubeconfig"}}],
}}))
PY
)"

log "running hub-probe on $MEMBER_CTX as $MEMBER_NS/$KSA"
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" delete pod hub-probe \
  --ignore-not-found >/dev/null 2>&1 || true
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" run hub-probe \
  --restart=Never --image "$IMAGE" --overrides="$OVERRIDES" >/dev/null

kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" wait --for=condition=Ready \
  pod/hub-probe --timeout=120s >/dev/null 2>&1 || true
# --follow rather than a plain read: the pod may still be starting, and this
# blocks until there is output instead of racing it.
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" logs -f pod/hub-probe || true
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" delete pod hub-probe \
  --ignore-not-found >/dev/null 2>&1 || true
