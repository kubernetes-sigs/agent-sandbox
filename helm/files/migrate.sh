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
#                       cluster-wide and, for each unique template referenced
#                       by a cold-start claim with no specific pool, creates
#                       a "shadow-pool-<template>" SandboxWarmPool (replicas=0)
#                       in that claim's namespace. The conversion webhook in
#                       v1beta1 redirects cold-start claims to that exact
#                       pool name; warm-started claims are left alone (the
#                       webhook derives their pool from the existing Sandbox).
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

readonly SHADOW_POOL_PREFIX="shadow-pool-"
readonly SHADOW_ANNOTATION_KEY="agents.x-k8s.io/migration-shadow"
readonly SHADOW_SOURCE_TEMPLATE_ANNOTATION_KEY="agents.x-k8s.io/migration-source-template"
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

# bootstrap_phase iterates all v1alpha1 SandboxClaims and ensures the
# conversion webhook has a valid warmPoolRef target for each. The webhook
# (extensions/api/v1alpha1/sandboxclaim_conversion.go) has three branches:
#
#   (A) spec.warmpool is a specific pool name -> webhook uses that name
#       verbatim. We verify the pool exists; if not, list it in the
#       operator-action summary at the end (don't fail; the webhook will
#       still rewrite the claim to point at the missing pool, and the
#       operator must create it manually).
#
#   (B) spec.warmpool in {"", "default"} AND the claim was warm-started
#       (status.sandboxStatus.name is non-empty and != claim name) ->
#       webhook uses stripRandomSuffix(sandboxName). The source pool
#       already exists (the sandbox came from it); no shadow needed.
#       ("none" never reaches this branch: claims with warmpool="none"
#       always cold-start, so they fall through to (C).)
#
#   (C) spec.warmpool in {"", "none", "default"} AND the claim is cold-
#       start (no sandbox, or sandbox.name == claim.name) -> webhook uses
#       shadow-pool-<template>. We collect a deduped set of (namespace,
#       template) pairs and create one shadow per pair, so claims sharing
#       a template share a single shadow pool.
#
# Idempotent: existing shadows are skipped; user-created pools that
# happen to share a shadow name are left alone (with a warning).
bootstrap_phase() {
  log "Bootstrap: scanning v1alpha1 SandboxClaims to identify pools needed..."

  local scanned=0 warmstart_skipped=0 specific_existing=0 created=0
  local skipped_existing_shadow=0 errors=0
  # Dedupe key "ns:template" -> any value indicates we need a shadow pool
  # for that (namespace, template) pair.
  declare -A needed_shadows=()
  # "ns/claim -> pool-name" entries for the operator-action summary printed
  # at the end. These are claims that named a specific pool that does not
  # currently exist; the webhook will rewrite the claim to point at that
  # name regardless, so the operator must create the pool manually.
  declare -a missing_specific_pools=()

  # Pull namespace, name, templateRef, warmpool policy, and the bound
  # sandbox name (if any) for every SandboxClaim in one shot. jsonpath
  # keeps us jq-free so the container image can be any minimal kubectl
  # image. status.sandboxStatus.name distinguishes warm-started (sandbox
  # exists and != claim name) from cold-start.
  #
  # We deliberately use '|' (a non-whitespace character) as the field
  # delimiter rather than tab. When IFS contains whitespace characters
  # like tab, bash's `read` collapses *consecutive* whitespace IFS chars
  # into a single delimiter (POSIX word-splitting rule), so an empty
  # spec.warmpool field (which is the common case under default
  # networkPolicyManagement) would shift sandbox_name into the warmpool
  # slot and corrupt the warm-start detection. '|' is non-whitespace, so
  # consecutive '|' characters are preserved as empty fields.
  #
  # Listing failures must abort the phase. Suppressing them silently would
  # make a missing-RBAC or transient API error look like "no claims found"
  # and exit 0, skipping the bootstrap entirely.
  local items
  # shellcheck disable=SC2046
  if ! items="$(kctl get sandboxclaims.extensions.agents.x-k8s.io $(ns_args) \
      -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"|"}{.spec.sandboxTemplateRef.name}{"|"}{.spec.warmpool}{"|"}{.status.sandboxStatus.name}{"\n"}{end}' \
      2>&1)"; then
    errlog "failed to list SandboxClaims: $items"
    errlog "check RBAC: ServiceAccount needs get/list on sandboxclaims.extensions.agents.x-k8s.io"
    return 1
  fi

  if [[ -z "$items" ]]; then
    log "Bootstrap: no SandboxClaims found. Nothing to do."
    return 0
  fi

  while IFS='|' read -r claim_ns claim_name template_name warmpool sandbox_name; do
    [[ -z "$claim_ns" ]] && continue
    scanned=$((scanned + 1))

    if [[ -z "$template_name" ]]; then
      warn "claim $claim_ns/$claim_name has no sandboxTemplateRef.name; cannot determine target pool, skipping"
      errors=$((errors + 1))
      continue
    fi

    case "$warmpool" in
      ""|"none"|"default")
        # Branch B vs C: warm-started or cold-start?
        if [[ -n "$sandbox_name" && "$sandbox_name" != "$claim_name" ]]; then
          # Warm-started. Webhook derives pool from sandbox name; that
          # pool already exists by virtue of having produced the sandbox.
          log "claim $claim_ns/$claim_name: warm-started (sandbox=$sandbox_name); no shadow needed"
          warmstart_skipped=$((warmstart_skipped + 1))
        else
          # Cold-start. Webhook will use shadow-pool-<template>. Dedupe
          # by (namespace, template) so multiple cold-start claims sharing
          # a template share a single shadow pool.
          needed_shadows["${claim_ns}:${template_name}"]=1
        fi
        ;;
      *)
        # Branch A: specific pool name. Webhook uses it verbatim. If the
        # pool doesn't exist, the webhook will still rewrite the claim to
        # point at it, but it won't be backed by anything - operator must
        # create the pool manually. We list these at the end of the phase
        # so the operator knows what work remains.
        local rc=0
        resource_exists sandboxwarmpools.extensions.agents.x-k8s.io "$claim_ns" "$warmpool" || rc=$?
        case "$rc" in
          0)
            log "claim $claim_ns/$claim_name: specific pool '$warmpool' exists, no action needed"
            specific_existing=$((specific_existing + 1))
            ;;
          1)
            warn "claim $claim_ns/$claim_name references non-existent pool '$warmpool'; needs manual creation post-migration"
            missing_specific_pools+=("$claim_ns/$claim_name -> $warmpool")
            ;;
          *)
            errlog "claim $claim_ns/$claim_name: transient error checking pool '$warmpool'; re-run to retry"
            errors=$((errors + 1))
            ;;
        esac
        ;;
    esac
  done <<< "$items"

  # Create the deduped shadow pools.
  local key ns template pool_name
  for key in "${!needed_shadows[@]}"; do
    ns="${key%%:*}"
    template="${key#*:}"
    pool_name="${SHADOW_POOL_PREFIX}${template}"

    # K8s names max 63 chars (DNS-1123 label). "shadow-pool-" is 12 chars,
    # so the template name can be up to 51 chars. Anything longer would
    # produce an invalid name; operator must rename the template or
    # pre-create a pool the webhook can reach.
    if (( ${#pool_name} > 63 )); then
      errlog "shadow pool name '$pool_name' exceeds K8s 63-char limit (template '$template' too long); operator must rename template or pre-create a pool"
      errors=$((errors + 1))
      continue
    fi

    local rc=0
    resource_exists sandboxwarmpools.extensions.agents.x-k8s.io "$ns" "$pool_name" || rc=$?
    case "$rc" in
      0)
        # Pool with that name already exists. Verify it's actually our
        # shadow and not a user pool that happens to share the name.
        # kubectl jsonpath does not reliably traverse annotation keys
        # containing "/" via dot-notation escaping, so we use go-template
        # with `index`. Missing annotation prints "<no value>" which is
        # != "true" - safe default.
        local existing_shadow
        existing_shadow="$(kctl get sandboxwarmpool -n "$ns" "$pool_name" \
          -o go-template="{{ index .metadata.annotations \"${SHADOW_ANNOTATION_KEY}\" }}" \
          2>/dev/null || true)"
        if [[ "$existing_shadow" == "true" ]]; then
          skipped_existing_shadow=$((skipped_existing_shadow + 1))
        else
          warn "pool $ns/$pool_name exists but is not marked as a migration shadow; not touching it"
          errors=$((errors + 1))
        fi
        continue
        ;;
      2)
        errors=$((errors + 1))
        continue
        ;;
    esac

    log "creating shadow pool $ns/$pool_name (for template $template)"
    if [[ "$DRY_RUN" == "true" ]]; then
      created=$((created + 1))
      continue
    fi

    local manifest
    manifest="$(cat <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: ${pool_name}
  namespace: ${ns}
  annotations:
    ${SHADOW_ANNOTATION_KEY}: "true"
    ${SHADOW_SOURCE_TEMPLATE_ANNOTATION_KEY}: "${template}"
spec:
  replicas: 0
  sandboxTemplateRef:
    name: ${template}
EOF
)"
    if printf '%s\n' "$manifest" | kctl apply -f - >/dev/null 2>&1; then
      created=$((created + 1))
    else
      errlog "failed to create shadow pool $ns/$pool_name"
      errors=$((errors + 1))
    fi
  done

  log "Bootstrap summary: scanned=$scanned warmstart_skipped=$warmstart_skipped specific_existing=$specific_existing created=$created skipped_existing_shadow=$skipped_existing_shadow errors=$errors"

  if (( ${#missing_specific_pools[@]} > 0 )); then
    warn ""
    warn "OPERATOR ACTION REQUIRED: the following SandboxClaims reference"
    warn "specific pools that do not currently exist. After migration, the"
    warn "conversion webhook will set warmPoolRef.name to those exact names,"
    warn "and you must create the pools manually for those claims to be"
    warn "satisfiable (or edit the claim's warmPoolRef to point elsewhere):"
    local line
    for line in "${missing_specific_pools[@]}"; do
      warn "  - $line"
    done
  fi

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
