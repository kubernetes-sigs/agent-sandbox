#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Emit a ConfigMap holding a CREDENTIAL-FREE kubeconfig for the hub cluster.
#
# WHY A CONFIGMAP AND NOT A SECRET
#
# The kubeconfig this writes contains exactly two things: the hub apiserver's
# address and its CA certificate. Both are public -- the CA is what clients use
# to verify the server, not something that authenticates anyone to it. There is
# no token, no client key, no service-account JSON.
#
# Authentication happens at runtime instead: the fleet-member and the planner
# run with `--hub-token-source=gke-metadata`, which asks the GKE metadata
# server for a token belonging to the GSA bound to the pod's KSA via Workload
# Identity. Nothing on disk expires, nothing needs rotating, and this file can
# be committed to git without a second thought.
#
# The usual alternative -- a per-member Secret holding a downloaded GSA key --
# means six long-lived private keys, six rotation schedules, and six chances to
# leak one. This exists to avoid that.
#
# USAGE
#
#   ./deploy/gen-hub-kubeconfig.sh \
#       --hub-cluster fleet-hub --hub-location us-central1 > hub-kubeconfig-cm.yaml
#   kubectl --context <member> apply -f hub-kubeconfig-cm.yaml
#
# Run once per MEMBER cluster (the ConfigMap is identical for all of them; only
# the cluster you apply it to changes).
#
# PREREQUISITES on each member cluster, which this script does NOT do:
#   1. The cluster has Workload Identity (--workload-pool=PROJECT.svc.id.goog).
#   2. A GSA exists per member and the hub grants it RBAC -- see gen-hub-rbac.py.
#   3. The KSA is bound to the GSA:
#        gcloud iam service-accounts add-iam-policy-binding \
#          fleet-member-<cluster>@PROJECT.iam.gserviceaccount.com \
#          --role roles/iam.workloadIdentityUser \
#          --member "serviceAccount:PROJECT.svc.id.goog[multi-cluster-fleet/fleet-member]"
#        kubectl annotate sa fleet-member -n multi-cluster-fleet \
#          iam.gke.io/gcp-service-account=fleet-member-<cluster>@PROJECT.iam.gserviceaccount.com
#   4. The member can actually REACH the address this writes. Two distinct
#      ways that fails, both of which present as a TLS connect timeout rather
#      than a 403 -- check reachability before suspecting auth:
#
#        a. The hub has authorized networks enabled and the member's egress IP
#           is not on the list.
#        b. The member's nodes have no external IP and the VPC has no Cloud
#           NAT, so the hub's PUBLIC endpoint is simply unroutable. Private
#           Google Access does not help here: it covers Google API endpoints
#           like storage.googleapis.com, and a GKE control plane's public IP
#           is not one. The tell is that the member's GCS writes keep working
#           while every hub call times out.
#
#      For (b) -- and it is the common case for a private fleet -- pass
#      --private-endpoint. When the hub and the members share a VPC, the
#      control plane's private endpoint is reachable by ordinary intra-VPC
#      routing: no NAT, no egress cost, and the hub is never exposed publicly.
#      The CA is the same either way; GKE's serving certificate carries both
#      endpoints as SANs, so TLS verification still succeeds.

set -euo pipefail

HUB_CLUSTER=""
HUB_LOCATION=""
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
NAMESPACE="${NAMESPACE:-multi-cluster-fleet}"
CM_NAME="${CM_NAME:-hub-kubeconfig}"
CONTEXT_NAME="${CONTEXT_NAME:-hub}"
USE_PRIVATE="false"

# Bounded by the `set -euo pipefail` line rather than a hardcoded number, so
# editing the header above cannot silently truncate --help.
usage() { sed -n '3,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//;/^set -euo/d'; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --hub-cluster)  HUB_CLUSTER="$2"; shift 2 ;;
    --hub-location) HUB_LOCATION="$2"; shift 2 ;;
    --project)      PROJECT="$2"; shift 2 ;;
    --namespace)    NAMESPACE="$2"; shift 2 ;;
    --name)         CM_NAME="$2"; shift 2 ;;
    --private-endpoint) USE_PRIVATE="true"; shift ;;
    -h|--help)      usage 0 ;;
    *) echo "unknown flag: $1" >&2; usage 1 ;;
  esac
done

[[ -n "$HUB_CLUSTER"  ]] || { echo "--hub-cluster is required" >&2; exit 1; }
[[ -n "$HUB_LOCATION" ]] || { echo "--hub-location is required" >&2; exit 1; }
[[ -n "$PROJECT"      ]] || { echo "no project; pass --project" >&2; exit 1; }

# `--location` covers both zonal and regional clusters, so callers do not have
# to know which the hub is.
#
# The separator is '|' and not a space on purpose: a cluster with no private
# endpoint yields an EMPTY middle field, and under the default IFS `read` would
# collapse the run of whitespace and shift the CA into PRIVATE_ENDPOINT. The
# resulting kubeconfig would carry a certificate where the address belongs,
# which fails as a confusing TLS error rather than a missing-field error.
IFS='|' read -r PUBLIC_ENDPOINT PRIVATE_ENDPOINT CA < <(
  gcloud container clusters describe "$HUB_CLUSTER" \
    --location "$HUB_LOCATION" --project "$PROJECT" \
    --format='value[separator="|"](endpoint, privateClusterConfig.privateEndpoint, masterAuth.clusterCaCertificate)'
)

if [[ "$USE_PRIVATE" == "true" ]]; then
  [[ -n "${PRIVATE_ENDPOINT:-}" ]] || {
    echo "--private-endpoint requested but ${HUB_CLUSTER} has no private endpoint" >&2
    exit 1
  }
  ENDPOINT="$PRIVATE_ENDPOINT"
  WHICH="private endpoint (intra-VPC)"
else
  ENDPOINT="$PUBLIC_ENDPOINT"
  WHICH="public endpoint"
fi

[[ -n "${ENDPOINT:-}" ]] || { echo "could not read hub endpoint" >&2; exit 1; }
[[ -n "${CA:-}"       ]] || { echo "could not read hub CA" >&2; exit 1; }

echo "[gen-hub-kubeconfig] using the ${WHICH}: ${ENDPOINT}" >&2

# The `user` stanza is intentionally EMPTY. load_kube_config needs the named
# user to exist so context resolution succeeds, but any credential here would
# be ignored -- hubauth.load_hub_configuration overrides authentication
# wholesale under gke-metadata. Leaving it empty makes that explicit rather
# than leaving a stale token to confuse the next reader.
cat <<YAML
# Generated by deploy/gen-hub-kubeconfig.sh -- do not edit by hand.
#
# Contains NO credentials: an apiserver address and a CA certificate, both
# public. Authentication is via Workload Identity at runtime
# (--hub-token-source=gke-metadata). Safe to commit.
#
#   hub cluster: ${HUB_CLUSTER} (${HUB_LOCATION}, project ${PROJECT})
#   address:     ${WHICH}
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: multi-cluster-fleet
data:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    current-context: ${CONTEXT_NAME}
    clusters:
      - name: ${CONTEXT_NAME}
        cluster:
          server: https://${ENDPOINT}
          certificate-authority-data: ${CA}
    users:
      - name: ${CONTEXT_NAME}
        user: {}
    contexts:
      - name: ${CONTEXT_NAME}
        context:
          cluster: ${CONTEXT_NAME}
          user: ${CONTEXT_NAME}
YAML
