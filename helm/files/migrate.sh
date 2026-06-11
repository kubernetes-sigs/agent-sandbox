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
#
# migrate.sh — agent-sandbox v1alpha1 -> v1beta1 migration helper.
#
# Two phases, both idempotent:
#
#   --phase=bootstrap   Pre-upgrade. Scans every v1alpha1 SandboxClaim
#                       cluster-wide and creates a per-claim "shadow"
#                       SandboxWarmPool (replicas=0) for claims that don't
#                       reference a specific existing pool. This gives the
#                       conversion webhook a valid warmPoolRef target when it
#                       converts those claims to v1beta1 later.
#
#   --phase=migrate     Post-upgrade. Patches every Sandbox / SandboxClaim /
#                       SandboxTemplate / SandboxWarmPool cluster-wide with
#                       a storage-migrated-at annotation, which forces the
#                       API server to read each resource and rewrite it to
#                       etcd in the v1beta1 storage format. Prevents stale
#                       v1alpha1 records from lingering and becoming
#                       unreadable when v1alpha1 is removed in a future
#                       release.
#
# Both phases tolerate per-resource failures (log + continue) and print a
# summary at the end. Both phases can be re-run safely.
#
# Usage:
#   migrate.sh --phase=bootstrap|migrate [--dry-run] [--namespace=<ns>]
#              [--kubectl=<path>]
#
# Environment variables (override CLI):
#   KUBECTL                 Path to kubectl binary. Default: kubectl.
#   MIGRATE_DRY_RUN         If "true", same effect as --dry-run.
#
# Documented entry point for operators: docs/api-migration-guide.md.

set -euo pipefail

# --- Config / constants -----------------------------------------------------

readonly SHADOW_POOL_SUFFIX="-shadow-pool"
readonly SHADOW_ANNOTATION_KEY="agents.x-k8s.io/migration-shadow"
readonly SHADOW_SOURCE_ANNOTATION_KEY="agents.x-k8s.io/migration-source-claim"
readonly MIGRATED_AT_ANNOTATION_KEY="agents.x-k8s.io/storage-migrated-at"

# CRDs are patched in this order to maximize the chance that, mid-migration,
# the cluster is internally consistent: pools and templates first (so any
# claim's warmPoolRef target is converted before the claim itself is touched),
# then sandboxes, then claims last.
readonly -a CRDS_TO_MIGRATE=(
  "sandboxwarmpools.extensions.agents.x-k8s.io"
  "sandboxtemplates.extensions.agents.x-k8s.io"
  "sandboxes.agents.x-k8s.io"
  "sandboxclaims.extensions.agents.x-k8s.io"
)

# --- CLI parsing ------------------------------------------------------------

PHASE=""
NAMESPACE=""              # empty = all namespaces
DRY_RUN="${MIGRATE_DRY_RUN:-false}"
KUBECTL="${KUBECTL:-kubectl}"

usage() {
  cat >&2 <<EOF
Usage: $0 --phase=bootstrap|migrate [--dry-run] [--namespace=<ns>] [--kubectl=<path>]

  --phase=PHASE     Required. One of: bootstrap, migrate.
  --dry-run         Print planned actions, do not modify any resources.
  --namespace=NS    Restrict to a single namespace. Default: all namespaces.
  --kubectl=PATH    Override kubectl binary path. Default: kubectl (or \$KUBECTL).
  -h, --help        Show this help.

See docs/api-migration-guide.md for full operator documentation.
EOF
}

for arg in "$@"; do
  case "$arg" in
    --phase=*)     PHASE="${arg#*=}" ;;
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --kubectl=*)   KUBECTL="${arg#*=}" ;;
    --dry-run)     DRY_RUN="true" ;;
    -h|--help)     usage; exit 0 ;;
    *)             echo "ERROR: unknown argument: $arg" >&2; usage; exit 2 ;;
  esac
done

if [[ -z "$PHASE" ]]; then
  echo "ERROR: --phase is required" >&2
  usage
  exit 2
fi

if [[ "$PHASE" != "bootstrap" && "$PHASE" != "migrate" ]]; then
  echo "ERROR: --phase must be one of: bootstrap, migrate (got: $PHASE)" >&2
  exit 2
fi

# --- Logging helpers --------------------------------------------------------

log()    { echo "[migrate:$PHASE] $*"; }
warn()   { echo "[migrate:$PHASE] WARN: $*" >&2; }
errlog() { echo "[migrate:$PHASE] ERROR: $*" >&2; }

# --- kubectl wrappers -------------------------------------------------------

# kubectl wrapper that respects --namespace if set; otherwise --all-namespaces
# for list operations.
kctl() { "$KUBECTL" "$@"; }

# ns_args echoes the namespace flag(s) for list operations.
ns_args() {
  if [[ -n "$NAMESPACE" ]]; then
    echo "-n $NAMESPACE"
  else
    echo "--all-namespaces"
  fi
}

# resource_exists checks if a specific named resource exists in a namespace.
# Returns 0 if exists, 1 if not, 2 on transient error.
resource_exists() {
  local kind="$1" ns="$2" name="$3"
  if kctl get "$kind" -n "$ns" "$name" >/dev/null 2>&1; then
    return 0
  fi
  # Distinguish NotFound from other errors so the caller can decide.
  local out
  out="$(kctl get "$kind" -n "$ns" "$name" 2>&1 || true)"
  if [[ "$out" == *"NotFound"* || "$out" == *"not found"* ]]; then
    return 1
  fi
  errlog "transient error checking $kind $ns/$name: $out"
  return 2
}

# --- Phase: bootstrap -------------------------------------------------------

# bootstrap_phase iterates all v1alpha1 SandboxClaims and ensures each has a
# corresponding SandboxWarmPool the conversion webhook can use as the
# v1beta1 warmPoolRef target. Idempotent: existing shadow pools are skipped;
# user-created pools with conflicting names are left alone (with a warning).
bootstrap_phase() {
  log "Bootstrap: scanning v1alpha1 SandboxClaims to identify pools needed..."

  local total=0 created=0 skipped_existing_shadow=0 skipped_user_pool=0 errors=0

  # Pull namespace, name, templateRef, and warmpool policy for every
  # SandboxClaim in one shot. jsonpath keeps us jq-free so the container
  # image can be any minimal kubectl image. Trailing blank fields are
  # preserved as empty strings between tabs.
  # Listing failures must abort the phase. Suppressing them silently would
  # make a missing-RBAC or transient API error look like "no claims found"
  # and exit 0, skipping the bootstrap entirely.
  local items
  # shellcheck disable=SC2046
  if ! items="$(kctl get sandboxclaims.extensions.agents.x-k8s.io $(ns_args) \
      -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.sandboxTemplateRef.name}{"\t"}{.spec.warmpool}{"\n"}{end}' \
      2>&1)"; then
    errlog "failed to list SandboxClaims: $items"
    errlog "check RBAC: ServiceAccount needs get/list on sandboxclaims.extensions.agents.x-k8s.io"
    return 1
  fi

  if [[ -z "$items" ]]; then
    log "Bootstrap: no SandboxClaims found. Nothing to do."
    return 0
  fi

  while IFS=$'\t' read -r claim_ns claim_name template_name warmpool; do
    [[ -z "$claim_ns" ]] && continue
    total=$((total + 1))

    # Decide what pool the v1beta1 warmPoolRef should target.
    local target_pool=""
    case "$warmpool" in
      ""|"none"|"default")
        target_pool="${claim_name}${SHADOW_POOL_SUFFIX}"
        ;;
      *)
        # User specified a specific pool name. resource_exists returns:
        #   0 = pool exists -> nothing to do
        #   1 = NotFound    -> mint a shadow (typo, deleted, etc.)
        #   2 = transient   -> log and skip (will retry on next run; do
        #                      NOT mint a shadow, because the pool may
        #                      actually exist and we just couldn't tell)
        local rc=0
        resource_exists sandboxwarmpools.extensions.agents.x-k8s.io "$claim_ns" "$warmpool" || rc=$?
        case "$rc" in
          0)
            log "claim $claim_ns/$claim_name: existing pool '$warmpool' found, no shadow needed"
            skipped_user_pool=$((skipped_user_pool + 1))
            continue
            ;;
          1)
            warn "claim $claim_ns/$claim_name references non-existent pool '$warmpool'; minting shadow pool"
            target_pool="${claim_name}${SHADOW_POOL_SUFFIX}"
            ;;
          *)
            errlog "claim $claim_ns/$claim_name: transient error checking pool '$warmpool'; skipping (re-run to retry)"
            errors=$((errors + 1))
            continue
            ;;
        esac
        ;;
    esac

    if [[ -z "$template_name" ]]; then
      warn "claim $claim_ns/$claim_name has no sandboxTemplateRef.name; cannot mint shadow pool, skipping"
      errors=$((errors + 1))
      continue
    fi

    # Check if shadow pool already exists.
    case "$(resource_exists sandboxwarmpools.extensions.agents.x-k8s.io "$claim_ns" "$target_pool"; echo $?)" in
      0)
        # Exists. Verify it's actually our shadow and not a user pool with the
        # same name (which could happen if the user named one *-shadow-pool).
        # kubectl jsonpath does not reliably traverse annotation keys
        # containing "/" via dot-notation escaping, so we use go-template
        # with `index` which handles arbitrary keys correctly. Missing
        # annotation prints "<no value>", which is != "true" - safe default.
        local existing_shadow
        existing_shadow="$(kctl get sandboxwarmpool -n "$claim_ns" "$target_pool" \
          -o go-template="{{ index .metadata.annotations \"${SHADOW_ANNOTATION_KEY}\" }}" \
          2>/dev/null || true)"
        if [[ "$existing_shadow" == "true" ]]; then
          skipped_existing_shadow=$((skipped_existing_shadow + 1))
        else
          warn "pool $claim_ns/$target_pool exists but is not marked as a migration shadow; not touching it"
          errors=$((errors + 1))
        fi
        continue
        ;;
      2)
        errors=$((errors + 1))
        continue
        ;;
    esac

    log "creating shadow pool $claim_ns/$target_pool for claim $claim_name (template: $template_name)"
    if [[ "$DRY_RUN" == "true" ]]; then
      created=$((created + 1))
      continue
    fi

    local manifest
    manifest="$(cat <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: ${target_pool}
  namespace: ${claim_ns}
  annotations:
    ${SHADOW_ANNOTATION_KEY}: "true"
    ${SHADOW_SOURCE_ANNOTATION_KEY}: "${claim_name}"
spec:
  replicas: 0
  sandboxTemplateRef:
    name: ${template_name}
EOF
)"
    if printf '%s\n' "$manifest" | kctl apply -f - >/dev/null 2>&1; then
      created=$((created + 1))
    else
      errlog "failed to create shadow pool $claim_ns/$target_pool"
      errors=$((errors + 1))
    fi
  done <<< "$items"

  log "Bootstrap summary: scanned=$total created=$created skipped_existing_shadow=$skipped_existing_shadow skipped_user_pool=$skipped_user_pool errors=$errors"
  if (( errors > 0 )); then
    warn "completed with $errors error(s); review log above"
    return 1
  fi
  return 0
}

# --- Phase: migrate ---------------------------------------------------------

# migrate_phase patches every resource of every migrated CRD with a benign
# annotation, which forces the API server to rewrite the resource through
# the conversion webhook in v1beta1 storage format. Idempotent.
migrate_phase() {
  log "Migrate: rewriting storage for all migrated CRDs..."
  local now
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  local total_success=0 total_failure=0
  for kind in "${CRDS_TO_MIGRATE[@]}"; do
    local k_success=0 k_failure=0
    log "processing $kind ..."

    local items
    # shellcheck disable=SC2046
    if ! items="$(kctl get "$kind" $(ns_args) \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}{end}' \
        2>&1)"; then
      errlog "  failed to list $kind: $items"
      errlog "  treating as failure for this CRD; continuing with other CRDs"
      total_failure=$((total_failure + 1))
      continue
    fi

    if [[ -z "$items" ]]; then
      log "  no resources found"
      continue
    fi

    while IFS=$'\t' read -r ns name; do
      [[ -z "$ns" ]] && continue

      if [[ "$DRY_RUN" == "true" ]]; then
        log "  DRY-RUN: would patch $kind $ns/$name"
        k_success=$((k_success + 1))
        continue
      fi

      local patch
      patch="{\"metadata\":{\"annotations\":{\"${MIGRATED_AT_ANNOTATION_KEY}\":\"${now}\"}}}"
      if kctl patch "$kind" -n "$ns" "$name" --type=merge -p "$patch" >/dev/null 2>&1; then
        k_success=$((k_success + 1))
      else
        errlog "  failed to patch $kind $ns/$name"
        k_failure=$((k_failure + 1))
      fi
    done <<< "$items"

    log "  $kind: success=$k_success failure=$k_failure"
    total_success=$((total_success + k_success))
    total_failure=$((total_failure + k_failure))
  done

  log "Migrate summary: total_success=$total_success total_failure=$total_failure"
  if (( total_failure > 0 )); then
    warn "completed with $total_failure failure(s); review log above and re-run if appropriate"
    return 1
  fi
  return 0
}

# --- Main dispatcher --------------------------------------------------------

log "agent-sandbox migration tool starting (kubectl=$KUBECTL, dry_run=$DRY_RUN, namespace=${NAMESPACE:-<all>})"

case "$PHASE" in
  bootstrap) bootstrap_phase ;;
  migrate)   migrate_phase ;;
esac

exit_code=$?
log "phase=$PHASE finished with exit_code=$exit_code"
exit "$exit_code"
