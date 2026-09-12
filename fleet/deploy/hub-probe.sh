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

log() { printf "\033[1;34m[probe]\033[0m %s\n" "$*" >&2; }
die() { printf "\033[1;31m[probe] ERROR:\033[0m %s\n" "$*" >&2; exit 1; }

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
CLUSTER_NAME="${CLUSTER_NAME:-cluster-a}"
ZONE="${ZONE:-us-central1-a}"
MEMBER_NS="${MEMBER_NS:-multi-cluster-fleet}"
HUB_NS="${HUB_NS:-fleet-system}"
KSA="${KSA:-fleet-member}"
IMAGE="${IMAGE:-curlimages/curl:8.8.0}"

# The context name is only derived when it was not given. Deriving it from an
# empty PROJECT yields "gke__us-central1-a_cluster-a", and kubectl then reports
# a missing context -- which reads as a kubeconfig problem rather than as an
# unset `gcloud config set project`.
if [[ -z "${MEMBER_CTX:-}" ]]; then
  [[ -n "$PROJECT" ]] || die "PROJECT is not set and gcloud has no default \
project. Set PROJECT=<id>, or set MEMBER_CTX=<context> directly."
  MEMBER_CTX="gke_${PROJECT}_${ZONE}_${CLUSTER_NAME}"
fi

# Alpine/busybox in the curl image: `grep -m1`, `sed`, `base64 -d` all exist.
# Deliberately no yq/jq -- adding a parser would mean a second image and a
# second thing that can be wrong.
read -r -d '' SCRIPT <<PROBE || true
set -u
M=http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default
KC=/etc/fleet-hub/kubeconfig

# Every request is bounded. This probe is reached for precisely when the hub
# path is broken, and the most common way for it to be broken -- a control
# plane whose public IP is not covered by Private Google Access -- is a silent
# blackhole, not a refusal. An unbounded curl there hangs forever: no output,
# no remaining checks, and a probe pod left running until someone notices.
# A timed-out request IS the result, so each one keeps going and prints.
CT="--connect-timeout 10 --max-time 30"

SRV=\$(grep -m1 'server:' \$KC | sed 's/.*server:[[:space:]]*//' | tr -d '"'"'"'"\r')
CA=\$(grep -m1 'certificate-authority-data:' \$KC | sed 's/.*certificate-authority-data:[[:space:]]*//' | tr -d '"'"'"'"\r')
echo "\$CA" | base64 -d > /tmp/ca.crt
# The one request whose failure is silent by construction. Piping straight into
# sed discards both the status and the exit code, and a 403, a 404 and a
# blackholed metadata server all leave TOK empty in exactly the same way. An
# empty bearer token then arrives at the hub as a 401 -- sending the operator to
# IAM and the hub's RBAC for a fault that never left this node. Report it like
# every other request, keep it non-fatal so the checks below still run, and say
# plainly that they cannot mean anything if it failed.
MD=\$(curl -s \$CT -o /tmp/token.json \
  -w 'http %{http_code} (curl %{exitcode})' \
  -H 'Metadata-Flavor: Google' "\$M/token") || true
# Match-or-nothing, not strip-and-hope. The old two-expression form ran on the
# body whatever it was: given metadata's 403 payload {"error":"forbidden"} the
# first expression finds no access_token and does nothing, the second truncates
# at the first quote, and TOK becomes a single open-brace character. Non-empty,
# so the check below stays quiet, and an Authorization header carrying that
# brace goes to the hub -- which answers 401, the exact wrong place to start
# looking. -n with a capture emits nothing unless there really was a token.
#
# (No backticks in this comment on purpose: PROBE is an unquoted here-document,
# so a backtick pair here would run as a command at generation time.)
TOK=\$(sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p' /tmp/token.json 2>/dev/null)

echo "server       : \$SRV"
echo "ca bytes     : \$(wc -c < /tmp/ca.crt)"
echo "metadata     : \$MD"
echo "token length : \$(printf %s "\$TOK" | wc -c)"
if [ -z "\$TOK" ]; then
  echo "  !! No token. Every authenticated check below will fail, and NOT"
  echo "     because of IAM or hub RBAC -- nothing ever left this node. A 403"
  echo "     here is the Workload Identity binding on the ${MEMBER_NS}/${KSA}"
  echo "     KSA; curl 28 or 7 is the metadata server itself."
fi
echo
echo "--- tokeninfo: what Google says this token is -----------------------"
curl -s \$CT -o /tmp/o0 -w 'http %{http_code} (curl %{exitcode})\n' \
  "https://oauth2.googleapis.com/tokeninfo?access_token=\$TOK" || true
head -c 700 /tmp/o0
echo; echo
echo "--- GET /version: is the hub reachable at all (no auth needed) ------"
curl -s \$CT -o /tmp/o1 -w 'http %{http_code} (curl %{exitcode})\n' \
  --cacert /tmp/ca.crt "\$SRV/version" || true
head -c 300 /tmp/o1
echo; echo
echo "--- SelfSubjectReview: who does the hub think we are ----------------"
curl -s \$CT -o /tmp/o3 -w 'http %{http_code} (curl %{exitcode})\n' \
  --cacert /tmp/ca.crt \
  -H "Authorization: Bearer \$TOK" -H 'Content-Type: application/json' \
  -X POST -d '{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}' \
  "\$SRV/apis/authentication.k8s.io/v1/selfsubjectreviews" || true
head -c 700 /tmp/o3
echo; echo
echo "--- GET the ClusterProfile: the call hubcheck actually makes --------"
curl -s \$CT -o /tmp/o2 -w 'http %{http_code} (curl %{exitcode})\n' \
  --cacert /tmp/ca.crt \
  -H "Authorization: Bearer \$TOK" \
  "\$SRV/apis/multicluster.x-k8s.io/v1alpha1/namespaces/${HUB_NS}/clusterprofiles/${CLUSTER_NAME}" || true
head -c 700 /tmp/o2
echo
echo
echo "curl exit 0 = the request completed; read the http code above it."
echo "curl exit 28 = timed out with no answer at all. On the hub calls that is"
echo "  the routing failure: a GKE control plane's public IP is not a Google"
echo "  API endpoint, so Private Google Access does not cover it. Look at"
echo "  master-authorized-networks and --enable-master-global-access, not IAM."
echo "curl exit 7  = refused/unreachable, which is a different fault: something"
echo "  answered. Wrong endpoint in the kubeconfig, or a NetworkPolicy."
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

cleanup() {
  kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" delete pod hub-probe \
    --ignore-not-found >/dev/null 2>&1 || true
}
# On the trap as well as at the end. The probe pod holds the member's identity
# and the operator running this is, by definition, already debugging; a Ctrl-C
# out of `logs -f` should not leave it behind.
trap cleanup EXIT INT TERM

log "running hub-probe on $MEMBER_CTX as $MEMBER_NS/$KSA"
cleanup
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" run hub-probe \
  --restart=Never --image "$IMAGE" --overrides="$OVERRIDES" >/dev/null

kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" wait --for=condition=Ready \
  pod/hub-probe --timeout=120s >/dev/null 2>&1 || true
# --follow rather than a plain read: the pod may still be starting, and this
# blocks until there is output instead of racing it.
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" logs -f pod/hub-probe || true
