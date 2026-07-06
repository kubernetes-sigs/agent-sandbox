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
OPENAPI_CMD="go run -modfile=tools.mod k8s.io/kube-openapi"
API_PKG_COMMAS="sigs.k8s.io/agent-sandbox/api/v1alpha1,sigs.k8s.io/agent-sandbox/api/v1beta1"
API_PKG_SPACES="sigs.k8s.io/agent-sandbox/api/v1alpha1 sigs.k8s.io/agent-sandbox/api/v1beta1"
CLIENT_PKG="sigs.k8s.io/agent-sandbox/clients/k8s"
APPLYCONFIG_PKG="${CLIENT_PKG}/applyconfiguration"
EXT_API_PKG_COMMAS="sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1,sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
EXT_API_PKG_SPACES="sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1 sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
EXT_CLIENT_PKG="sigs.k8s.io/agent-sandbox/clients/k8s/extensions"
EXT_APPLYCONFIG_PKG="${EXT_CLIENT_PKG}/applyconfiguration"
OPENAPI_WORK_DIR=""

cleanup_openapi_work() {
  if [[ -n "${OPENAPI_WORK_DIR}" && -d "${OPENAPI_WORK_DIR}" ]]; then
    rm -rf "${OPENAPI_WORK_DIR}"
  fi
  if [[ -n "${OPENAPI_SCHEMA:-}" ]]; then
    rm -f "${OPENAPI_SCHEMA}"
  fi
}

trap cleanup_openapi_work EXIT

OPENAPI_SCHEMA="${SCRIPT_ROOT}/bin/client-gen-openapi-schema.json"
OPENAPI_WORK_DIR="$(mktemp -d "${SCRIPT_ROOT}/zz_client_openapi_work_XXXXXX")"
OPENAPI_WORK_BASENAME="$(basename "${OPENAPI_WORK_DIR}")"
OPENAPI_DEFS_PKG="sigs.k8s.io/agent-sandbox/${OPENAPI_WORK_BASENAME}/defs"
OPENAPI_DUMP_DIR="${OPENAPI_WORK_DIR}/dump"
OPENAPI_DEFS_DIR="${OPENAPI_WORK_DIR}/defs"
OPENAPI_REPORT="${OPENAPI_WORK_DIR}/api_violations.report"

echo "Generating OpenAPI schema for apply configurations..."
mkdir -p "${OPENAPI_DEFS_DIR}" "${OPENAPI_DUMP_DIR}" "$(dirname "${OPENAPI_SCHEMA}")"
${OPENAPI_CMD}/cmd/openapi-gen \
  --output-dir "${OPENAPI_DEFS_DIR}" \
  --output-pkg "${OPENAPI_DEFS_PKG}" \
  --output-file zz_generated.openapi.go \
  --report-filename "${OPENAPI_REPORT}" \
  k8s.io/apimachinery/pkg/apis/meta/v1 \
  k8s.io/apimachinery/pkg/runtime \
  k8s.io/apimachinery/pkg/version \
  k8s.io/apimachinery/pkg/api/resource \
  k8s.io/apimachinery/pkg/util/intstr \
  k8s.io/api/core/v1 \
  k8s.io/api/networking/v1 \
  ${API_PKG_SPACES} \
  ${EXT_API_PKG_SPACES}

cat > "${OPENAPI_DUMP_DIR}/main.go" <<EOF
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"k8s.io/kube-openapi/pkg/builder"
	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/util"
	"k8s.io/kube-openapi/pkg/validation/spec"

	generated "${OPENAPI_DEFS_PKG}"
)

func main() {
	swagger, err := builder.BuildOpenAPIDefinitionsForResources(&common.Config{
		Info: &spec.Info{InfoProps: spec.InfoProps{Title: "agent-sandbox", Version: "v0"}},
		GetDefinitions: generated.GetOpenAPIDefinitions,
		GetDefinitionName: func(name string) (string, spec.Extensions) {
			return util.ToRESTFriendlyName(name), nil
		},
	},
		"sigs.k8s.io/agent-sandbox/api/v1alpha1.Sandbox",
		"sigs.k8s.io/agent-sandbox/api/v1beta1.Sandbox",
		"sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1.SandboxClaim",
		"sigs.k8s.io/agent-sandbox/extensions/api/v1beta1.SandboxClaim",
		"sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1.SandboxTemplate",
		"sigs.k8s.io/agent-sandbox/extensions/api/v1beta1.SandboxTemplate",
		"sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1.SandboxWarmPool",
		"sigs.k8s.io/agent-sandbox/extensions/api/v1beta1.SandboxWarmPool",
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build openapi schema: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(swagger); err != nil {
		fmt.Fprintf(os.Stderr, "encode openapi schema: %v\n", err)
		os.Exit(1)
	}
}
EOF
go run -modfile=tools.mod "./${OPENAPI_WORK_BASENAME}/dump" > "${OPENAPI_SCHEMA}"

echo "Generating apply configurations..."
${CMD}/cmd/applyconfiguration-gen \
  --output-dir "clients/k8s/applyconfiguration" \
  --output-pkg "${APPLYCONFIG_PKG}" \
  --openapi-schema "${OPENAPI_SCHEMA}" \
  ${API_PKG_SPACES}

echo "Generating clientset..."
${CMD}/cmd/client-gen \
  --output-dir "clients/k8s/clientset" \
  --output-pkg "${CLIENT_PKG}/clientset" \
  --clientset-name "versioned" \
  --apply-configuration-package "${APPLYCONFIG_PKG}" \
  --input-base "" \
  --input "${API_PKG_COMMAS}"

echo "Generating listers..."
${CMD}/cmd/lister-gen \
  --output-dir "clients/k8s/listers" \
  --output-pkg "${CLIENT_PKG}/listers" \
  ${API_PKG_SPACES}

echo "Generating informers..."
${CMD}/cmd/informer-gen \
  --output-dir "clients/k8s/informers" \
  --output-pkg "${CLIENT_PKG}/informers" \
  --versioned-clientset-package "${CLIENT_PKG}/clientset/versioned" \
  --listers-package "${CLIENT_PKG}/listers" \
  ${API_PKG_SPACES}


echo "Generating extensions apply configurations..."
${CMD}/cmd/applyconfiguration-gen \
  --output-dir "clients/k8s/extensions/applyconfiguration" \
  --output-pkg "${EXT_APPLYCONFIG_PKG}" \
  --openapi-schema "${OPENAPI_SCHEMA}" \
  ${EXT_API_PKG_SPACES}

echo "Generating extensions clientset..."
${CMD}/cmd/client-gen \
  --output-dir "clients/k8s/extensions/clientset" \
  --output-pkg "${EXT_CLIENT_PKG}/clientset" \
  --clientset-name "versioned" \
  --apply-configuration-package "${EXT_APPLYCONFIG_PKG}" \
  --input-base "" \
  --input "${EXT_API_PKG_COMMAS}"

echo "Generating extensions listers..."
${CMD}/cmd/lister-gen \
  --output-dir "clients/k8s/extensions/listers" \
  --output-pkg "${EXT_CLIENT_PKG}/listers" \
  ${EXT_API_PKG_SPACES}

echo "Generating extensions informers..."
${CMD}/cmd/informer-gen \
  --output-dir "clients/k8s/extensions/informers" \
  --output-pkg "${EXT_CLIENT_PKG}/informers" \
  --versioned-clientset-package "${EXT_CLIENT_PKG}/clientset/versioned" \
  --listers-package "${EXT_CLIENT_PKG}/listers" \
  ${EXT_API_PKG_SPACES}

cleanup_openapi_work

echo "Fixing license headers..."
"${SCRIPT_ROOT}"/dev/tools/fix-boilerplate

echo "Done."
