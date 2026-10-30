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
while read -r image; do
  [[ -z "$image" ]] && continue
  path=$image
  case "$path" in
    */*) ;;
    *) path="library/$path" ;;
  esac
  path=${path#docker.io/}
  if crane copy "$image" "$mirror/$path" >/dev/null 2>&1; then
    echo "ok   $image"
  else
    echo "FAIL $image"; fail=$((fail + 1))
  fi
done <<< "$images"
echo "done, $fail failures"
exit $(( fail > 0 ))
