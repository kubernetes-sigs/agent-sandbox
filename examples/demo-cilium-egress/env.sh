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

# Shared configuration for the Cilium per-identity egress demo.
# Override any value by exporting it before sourcing, e.g. `ZONE=us-west1-a ./scripts/setup-all.sh`.

# Defaults to your active gcloud project; export PROJECT to override. There is no
# baked-in project so the scripts never operate on an unexpected one.
export PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
export ZONE="${ZONE:-us-central1-a}"
export CLUSTER="${CLUSTER:-agent-sandbox-cilium-demo}"
export MACHINE_TYPE="${MACHINE_TYPE:-e2-standard-4}"
export NUM_NODES="${NUM_NODES:-2}"
export RELEASE_CHANNEL="${RELEASE_CHANNEL:-regular}"

# The demo namespace is hard-coded in manifests/*.yaml; keep these in sync if you change it.
export NAMESPACE="${NAMESPACE:-sandbox-demo}"

# Public repo the sandboxes try to clone to prove egress.
export TEST_REPO_URL="${TEST_REPO_URL:-https://github.com/rtyley/small-test-repo}"

# agent-sandbox release tag, or "latest" to auto-discover the newest GitHub release.
export AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-latest}"

# Optional: pin a Cilium chart version. Empty => cilium CLI default.
export CILIUM_VERSION="${CILIUM_VERSION:-}"
