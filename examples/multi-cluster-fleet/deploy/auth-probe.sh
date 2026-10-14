#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Print the HTTP headers our Python client actually sends to the hub.
#
#   FLEET_MEMBER_IMAGE=<ref> ./deploy/auth-probe.sh
#
# deploy/hub-probe.sh established that raw curl with the pod's Workload
# Identity token gets 200 from the hub while fleet-hubcheck gets 401 on the
# same call. That narrows the fault to our client, and leaves exactly one
# question: is the Authorization header missing, or present and malformed?
#
# `Configuration.api_key` being populated does NOT answer this. The header is
# assembled much later, and several layers between here and the socket can
# drop it. So this monkeypatches the REST layer and dumps the real headers of
# the real request, then makes the call and reports what came back.
#
# Uses the fleet image, so it needs no rebuild -- hubauth.py is already
# installed in it and this imports the same code path hubcheck uses.

set -euo pipefail

log() { printf "\033[1;34m[authprobe]\033[0m %s\n" "$*" >&2; }
die() { printf "\033[1;31m[authprobe] ERROR:\033[0m %s\n" "$*" >&2; exit 1; }

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
CLUSTER_NAME="${CLUSTER_NAME:-cluster-a}"
ZONE="${ZONE:-us-central1-a}"
MEMBER_NS="${MEMBER_NS:-multi-cluster-fleet}"
HUB_NS="${HUB_NS:-fleet-system}"
KSA="${KSA:-fleet-member}"

# Only derived when not given: from an empty PROJECT this would produce
# "gke__us-central1-a_cluster-a" and kubectl would report a missing context,
# which reads as a kubeconfig problem rather than an unset gcloud project.
if [[ -z "${MEMBER_CTX:-}" ]]; then
  [[ -n "$PROJECT" ]] || die "PROJECT is not set and gcloud has no default \
project. Set PROJECT=<id>, or set MEMBER_CTX=<context> directly."
  MEMBER_CTX="gke_${PROJECT}_${ZONE}_${CLUSTER_NAME}"
fi

# Any image that carries the fleet member's Python environment: it must have
# agent_sandbox_fleet importable and the `kubernetes` client installed, since
# this probe imports hubauth.py and monkeypatches the REST layer. The image the
# fleet member is already running is the right one -- using a different build
# would be probing a different client than the one that is failing.
[[ -n "${FLEET_MEMBER_IMAGE:-}" ]] || die "set FLEET_MEMBER_IMAGE to the fleet \
member image (the same ref deployment/fleet-member runs; deploy/build-push.sh \
prints one). Read it off the running member with:
  kubectl --context \"\$MEMBER_CTX\" -n $MEMBER_NS get deploy/fleet-member \\
    -o jsonpath='{.spec.template.spec.containers[0].image}'"

PY=$(cat <<'PYEOF'
import json, sys, traceback
import kubernetes
from kubernetes.client import rest
from agent_sandbox_fleet.hubauth import load_hub_configuration

KC = "/etc/fleet-hub/kubeconfig"
NS = "__HUB_NS__"
NAME = "__CLUSTER_NAME__"

print("kubernetes client :", getattr(kubernetes, "__version__", "?"))

cfg = load_hub_configuration(kubeconfig=KC, token_source="gke-metadata")
print("host              :", cfg.host)
print("api_key keys      :", sorted(cfg.api_key))
print("api_key_prefix    :", dict(cfg.api_key_prefix))
print("refresh hook set  :", cfg.refresh_api_key_hook is not None)

# auth_settings() is what the generated client consults per request. If
# 'BearerToken' is absent here, no amount of api_key population matters.
try:
    settings = cfg.auth_settings()
    print("auth_settings     :", list(settings))
    for k, v in settings.items():
        val = v.get("value") or ""
        print("   %s: in=%s key=%s len=%d value=%r..." %
              (k, v.get("in"), v.get("key"), len(val), val[:14]))
except Exception as e:
    print("auth_settings RAISED:", e)

# The decisive part: what goes on the wire.
orig = rest.RESTClientObject.request
def spy(self, method, url, *a, **kw):
    hdrs = dict(kw.get("headers") or {})
    for k in list(hdrs):
        if k.lower() == "authorization":
            hdrs[k] = "%s... (%d chars)" % (hdrs[k][:20], len(hdrs[k]))
    print("\n--- outgoing request ---")
    print("  %s %s" % (method, url))
    print("  headers: %s" % json.dumps(hdrs, indent=4, sort_keys=True))
    return orig(self, method, url, *a, **kw)
rest.RESTClientObject.request = spy

api = kubernetes.client.CustomObjectsApi(
    kubernetes.client.ApiClient(configuration=cfg))
try:
    obj = api.get_namespaced_custom_object(
        group="multicluster.x-k8s.io", version="v1alpha1",
        namespace=NS, plural="clusterprofiles", name=NAME)
    print("\nRESULT: 200, got %s/%s" %
          (obj.get("kind"), (obj.get("metadata") or {}).get("name")))
except Exception as e:
    print("\nRESULT: %s" % getattr(e, "status", type(e).__name__))
    body = getattr(e, "body", None)
    if body:
        print(str(body)[:300])
    else:
        traceback.print_exc()
PYEOF
)
PY="${PY//__HUB_NS__/$HUB_NS}"
PY="${PY//__CLUSTER_NAME__/$CLUSTER_NAME}"

OVERRIDES="$(PY="$PY" KSA="$KSA" IMAGE="$FLEET_MEMBER_IMAGE" python3 - <<'PY2'
import json, os
print(json.dumps({"spec": {
    "serviceAccountName": os.environ["KSA"],
    "restartPolicy": "Never",
    "containers": [{
        "name": "auth-probe",
        "image": os.environ["IMAGE"],
        "imagePullPolicy": "Always",
        "command": ["python", "-u", "-c", os.environ["PY"]],
        "volumeMounts": [{"name": "hub", "mountPath": "/etc/fleet-hub",
                          "readOnly": True}],
    }],
    "volumes": [{"name": "hub", "configMap": {"name": "hub-kubeconfig"}}],
}}))
PY2
)"

cleanup() {
  kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" delete pod auth-probe \
    --ignore-not-found >/dev/null 2>&1 || true
}
# The probe pod runs as the member's ServiceAccount and prints a redacted
# bearer token. A Ctrl-C out of `logs -f` should not leave that sitting there.
trap cleanup EXIT INT TERM

log "running auth-probe on $MEMBER_CTX as $MEMBER_NS/$KSA"
cleanup
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" run auth-probe \
  --restart=Never --image "$FLEET_MEMBER_IMAGE" --overrides="$OVERRIDES" >/dev/null
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" wait --for=condition=Ready \
  pod/auth-probe --timeout=120s >/dev/null 2>&1 || true
kubectl --context "$MEMBER_CTX" -n "$MEMBER_NS" logs -f pod/auth-probe || true
