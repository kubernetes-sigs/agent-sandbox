#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Build the fleet-member image and push it to Artifact Registry, creating the
# repository if it does not exist.
#
#   ./deploy/build-push.sh                 # tag = src-<hash of the sources>
#   TAG=hubcheck ./deploy/build-push.sh    # override (see the warning below)
#
# Prints the full image reference on stdout (and nothing else on stdout), so it
# composes:
#
#   export FLEET_MEMBER_IMAGE=$(./deploy/build-push.sh)
#
# The build context is the agent-sandbox repo ROOT, not this directory: the
# Dockerfile copies both the fleet package and the sibling Python SDK so pip
# can resolve k8s-agent-sandbox from source.
#
# WARNING on overriding TAG: a fixed tag you push to twice is a trap. Nodes
# cache by tag, so the second push does not reach a node that already ran the
# first. Override only for a throwaway you will pull with imagePullPolicy:
# Always.

set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
REGION="${REGION:-us-central1}"
REPO="${REPO:-agent-sandbox}"
IMAGE="${IMAGE:-fleet-member}"
PLATFORM="${PLATFORM:-linux/amd64}"

POC_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$POC_ROOT/../.." && pwd)"

# Everything informational goes to stderr so stdout stays a single image ref.
log() { printf "\033[1;34m[build]\033[0m %s\n" "$*" >&2; }
die() { printf "\033[1;31m[build] ERROR:\033[0m %s\n" "$*" >&2; exit 1; }

[[ -n "$PROJECT" ]] || die "no project; set PROJECT or run gcloud config set project"

# CONTENT-ADDRESSED TAG, deliberately not the git SHA.
#
# Two reasons, one of which already cost a debugging cycle:
#
#   1. fleet/ is currently untracked, so a git SHA says
#      nothing whatsoever about the code in this image.
#   2. A tag that does not change when the code does is actively dangerous
#      with imagePullPolicy: IfNotPresent. The node keeps its cached layer,
#      the pod runs the OLD binary, and the symptom is a bug you already
#      fixed reappearing — which reads as "my fix didn't work" rather than
#      "this isn't my fix".
#
# Hashing the actual build inputs makes the tag change exactly when the image
# would, so IfNotPresent becomes correct instead of a trap.
#
# sha256sum on Linux, shasum -a 256 on macOS. The tag is only compared against
# itself so the two need not agree across machines, but a build from a Mac and
# a build from the Linux box will produce different tags for identical sources.
# That is a wasted push, never a wrong image.
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum; }
else
  sha256() { shasum -a 256; }
fi

# Relative paths, so the tag depends on the sources and not on where the repo
# happens to be checked out. Both the NAME stream and the CONTENT stream are
# hashed: contents alone would miss a pure rename, which changes imports.
sources() {
  find fleet/python \
       fleet/deploy/Dockerfile \
       clients/python/agentic-sandbox-client \
       -type f \
       ! -path '*/.venv/*' ! -path '*/__pycache__/*' \
       ! -path '*/.pytest_cache/*' ! -name '*.pyc' \
       -print0 2>/dev/null | sort -z
}

if [[ -z "${TAG:-}" ]]; then
  TAG="$(
    cd "$REPO_ROOT" || exit 1
    { sources; sources | xargs -0 cat; } | sha256 | cut -c1-12
  )"
  [[ ${#TAG} -eq 12 ]] || die "could not compute a content tag (got ${TAG:-empty})"
  TAG="src-${TAG}"
fi

HOST="${REGION}-docker.pkg.dev"
REF="${HOST}/${PROJECT}/${REPO}/${IMAGE}:${TAG}"

if ! gcloud artifacts repositories describe "$REPO" \
      --location "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
  log "creating Artifact Registry repo $REPO in $REGION"
  gcloud artifacts repositories create "$REPO" \
    --repository-format=docker --location "$REGION" --project "$PROJECT" \
    --description "agent-sandbox multi-cluster fleet images" >&2
fi

log "configuring docker auth for $HOST"
gcloud auth configure-docker "$HOST" --quiet >&2

# --platform matters: nodes are amd64, and a build from an arm64 workstation
# produces an image that pulls fine and then dies with `exec format error`,
# which surfaces as CrashLoopBackOff with no useful log line.
log "building $REF (platform $PLATFORM)"
docker build --platform "$PLATFORM" \
  -t "$REF" -f "$POC_ROOT/deploy/Dockerfile" "$REPO_ROOT" >&2

log "pushing"
docker push "$REF" >&2

log "done"
echo "$REF"
