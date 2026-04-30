#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELM_DIR="${ROOT_DIR}/helm"
OPERATOR_DIR="${ROOT_DIR}/operator-sdk"

CRD_SRC_DIR="${HELM_DIR}/crds"
CRD_DST_DIR="${OPERATOR_DIR}/config/crd/bases"

RBAC_BASE_FILE="${HELM_DIR}/templates/rbac.generated.yaml"
RBAC_EXT_FILE="${HELM_DIR}/templates/extensions-rbac.generated.yaml"
RBAC_DST_FILE="${OPERATOR_DIR}/config/rbac/role.yaml"

cp "${CRD_SRC_DIR}/"*.yaml "${CRD_DST_DIR}/"

tmp_file="$(mktemp)"
{
  cat <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
EOF
  awk 'found { print } /^rules:/ { found=1; next }' "${RBAC_BASE_FILE}"
  awk 'found { print } /^rules:/ { found=1; next }' "${RBAC_EXT_FILE}"
} > "${tmp_file}"

mv "${tmp_file}" "${RBAC_DST_FILE}"
echo "Synced CRDs and RBAC from helm/ into operator-sdk/config"
