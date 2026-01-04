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


set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/../..
cd "${SCRIPT_ROOT}"

CMD="go run -modfile=tools.mod k8s.io/code-generator"
API_PKG="sigs.k8s.io/agent-sandbox/api/v1alpha1"
CLIENT_PKG="sigs.k8s.io/agent-sandbox/clients/k8s"

echo "Generating clientset..."
${CMD}/cmd/client-gen \
  --output-dir "clients/k8s/clientset" \
  --output-pkg "${CLIENT_PKG}/clientset" \
  --clientset-name "versioned" \
  --input-base "" \
  --input "${API_PKG}"

echo "Generating listers..."
${CMD}/cmd/lister-gen \
  --output-dir "clients/k8s/listers" \
  --output-pkg "${CLIENT_PKG}/listers" \
  "${API_PKG}"

echo "Generating informers..."
${CMD}/cmd/informer-gen \
  --output-dir "clients/k8s/informers" \
  --output-pkg "${CLIENT_PKG}/informers" \
  --versioned-clientset-package "${CLIENT_PKG}/clientset/versioned" \
  --listers-package "${CLIENT_PKG}/listers" \
  "${API_PKG}"


EXT_API_PKG="sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
EXT_CLIENT_PKG="sigs.k8s.io/agent-sandbox/clients/k8s/extensions"

echo "Generating extensions clientset..."
${CMD}/cmd/client-gen \
  --output-dir "clients/k8s/extensions/clientset" \
  --output-pkg "${EXT_CLIENT_PKG}/clientset" \
  --clientset-name "versioned" \
  --input-base "" \
  --input "${EXT_API_PKG}"

echo "Generating extensions listers..."
${CMD}/cmd/lister-gen \
  --output-dir "clients/k8s/extensions/listers" \
  --output-pkg "${EXT_CLIENT_PKG}/listers" \
  "${EXT_API_PKG}"

echo "Generating extensions informers..."
${CMD}/cmd/informer-gen \
  --output-dir "clients/k8s/extensions/informers" \
  --output-pkg "${EXT_CLIENT_PKG}/informers" \
  --versioned-clientset-package "${EXT_CLIENT_PKG}/clientset/versioned" \
  --listers-package "${EXT_CLIENT_PKG}/listers" \
  "${EXT_API_PKG}"

echo "Done."
