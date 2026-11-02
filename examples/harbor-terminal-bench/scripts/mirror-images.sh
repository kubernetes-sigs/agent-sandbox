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

# Copy a Harbor dataset's task images from Docker Hub into a registry you
# control, so a 50-wide run does not hit Hub pull limits and, on GKE, Image
# Streaming can serve them. Then run with --ek image_mirror=<prefix>.
#
#   scripts/mirror-images.sh <mirror-prefix> [dataset-git-url] [git-ref]
#   scripts/mirror-images.sh us-central1-docker.pkg.dev/my-proj/hub-mirror
#
# Needs git and crane (https://github.com/google/go-containerregistry).
set -euo pipefail

mirror=${1:?mirror prefix, e.g. us-central1-docker.pkg.dev/my-proj/hub-mirror}
repo=${2:-https://github.com/laude-institute/terminal-bench-2.git}
ref=${3:-main}
command -v crane >/dev/null || { echo "crane is required"; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git clone -q --depth 1 --branch "$ref" "$repo" "$work/tasks"

# docker_image = "alexgshaw/<task>:<tag>" in each task.toml.
images=$(grep -rhoE '^docker_image\s*=\s*"[^"]+"' "$work/tasks" --include=task.toml | sed -E 's/.*"([^"]+)"/\1/' | sort -u)
count=$(echo "$images" | grep -c . || true)
echo "mirroring $count images to $mirror"
fail=0
# Canonical source reference: Docker Hub shorthand expanded, so that
# python:3.13 and docker.io/library/python:3.13 are one image, not two.
canonical_source() {
  local image=$1 first rest
  first=${image%%/*}
  rest=${image#*/}
  if [[ "$image" == */* && ( "$first" == *.* || "$first" == *:* || "$first" == localhost ) ]]; then
    case "$first" in
      docker.io|index.docker.io|registry-1.docker.io)
        [[ "$rest" == */* ]] || rest="library/$rest"
        printf 'docker.io/%s' "$rest" ;;
      *) printf '%s' "$image" ;;
    esac
  else
    [[ "$image" == */* ]] || image="library/$image"
    printf 'docker.io/%s' "$image"
  fi
}

# Destination path = the path the integration's rewrite_registry() requests:
# the registry host is dropped, and a Docker Hub image without a namespace
# gets "library/" (so python:3.13 -> library/python:3.13).
mirror_path() {
  local image=$1 first rest path
  first=${image%%/*}
  rest=${image#*/}
  if [[ "$image" == */* && ( "$first" == *.* || "$first" == *:* || "$first" == localhost ) ]]; then
    path=$rest
    case "$first" in
      docker.io|index.docker.io|registry-1.docker.io)
        [[ "$path" == */* ]] || path="library/$path" ;;
    esac
  else
    path=$image
    [[ "$path" == */* ]] || path="library/$path"
  fi
  printf '%s' "$path"
}

# Aliases of one image (python:3.13, docker.io/library/python:3.13) are copied
# once. Two different sources that map to one destination (the same path
# under two registries) would overwrite each other: refuse before any copy.
sources=$(while read -r image; do [[ -n "$image" ]] && printf '%s %s\n' "$(canonical_source "$image")" "$image"; done <<< "$images" | sort -u -k1,1)
collisions=$(while read -r canon image; do printf '%s %s\n' "$(mirror_path "$image")" "$canon"; done <<< "$sources" \
  | sort | awk '{ if ($1 == prev) { print prevline; print $0 } prev = $1; prevline = $0 }' | sort -u)
if [[ -n "$collisions" ]]; then
  echo "refusing to mirror: these images map to the same destination path:" >&2
  echo "$collisions" >&2
  exit 1
fi

while read -r canon image; do
  [[ -z "$image" ]] && continue
  path=$(mirror_path "$image")
  if crane copy "$image" "$mirror/$path" >/dev/null 2>&1; then
    echo "ok   $image"
  else
    echo "FAIL $image"; fail=$((fail + 1))
  fi
done <<< "$sources"
echo "done, $fail failures"
exit $(( fail > 0 ))
