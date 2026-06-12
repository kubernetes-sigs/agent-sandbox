# v1alpha1 → v1beta1 API migration guide

This document covers the operational side of migrating `Sandbox`, `SandboxClaim`, `SandboxTemplate`, and `SandboxWarmPool` resources from the `v1alpha1` API to the `v1beta1` API.

If you install the chart fresh with the v1beta1-storage version, there is nothing to migrate — read this only when **upgrading** an existing installation that holds v1alpha1-serialized resources in etcd.

## What changes between versions

Most CRDs are schema-compatible across the two versions; the migration matters mainly for **two reasons**:

1. **`SandboxClaim` is not field-compatible.** v1alpha1 has `spec.sandboxTemplateRef` plus an optional `spec.warmpool` string policy (`"none"` / `"default"` / a specific pool name). v1beta1 requires `spec.warmPoolRef.name`. The conversion webhook (in `extensions/api/v1alpha1/sandboxclaim_conversion.go`) handles the rewrite via three branches:
   - **Specific pool name** (`warmpool: my-pool`) → webhook uses that name verbatim. If the pool doesn't exist, the converted claim points at a missing pool — operator must create it.
   - **`""` / `"default"`, warm-started** (claim has a bound `Sandbox` whose name differs from the claim's name) → webhook derives the pool name from the existing `Sandbox` via `stripRandomSuffix(sandboxName)`. The source pool already exists; nothing to do at migration time. (`"none"` never falls into this branch — `"none"` always cold-starts.)
   - **`""` / `"none"` / `"default"`, cold-start** (no bound `Sandbox`, or `Sandbox.name == claim.name`) → webhook redirects to `shadow-pool-<template-name>`. The bootstrap phase ensures one such shadow pool exists per `(namespace, template)` combination.
2. **`Sandbox.spec.replicas` becomes `Sandbox.spec.operatingMode`.** `replicas: 0` → `Suspended`, `replicas: 1` (or unset) → `Running`. The webhook handles this automatically.

The other two CRDs (`SandboxTemplate`, `SandboxWarmPool`) are structurally identical between versions but still need a storage rewrite so etcd holds them in v1beta1 form.

## Two phases

The migration script has two phases, run in this order:

- **`--phase=bootstrap`** — must run **before** the v1beta1 CRDs/controller are applied. Pre-creates any `shadow-pool-<template>` pools needed by the conversion webhook so cold-start claims have a valid `warmPoolRef` target. Operates on the v1alpha1 API.
- **`--phase=migrate`** — must run **after** the v1beta1 CRDs and conversion webhook are live. Patches every existing resource with a benign annotation, forcing the API server to read it through the conversion webhook and rewrite it to etcd in v1beta1 storage format.

Both phases are idempotent — safe to re-run.

## Before you start: back up your data

Before running either phase, dump every CR the migration will touch so you have a known-good snapshot to fall back to if anything goes wrong:

```bash
kubectl get sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools \
  -A -o yaml > agent-sandbox-backup-$(date -u +%Y%m%dT%H%M%SZ).yaml
```

Keep the file somewhere durable (not on a worker pod that may get rescheduled). Useful for:

- Inspecting the original v1alpha1 shape if a converted v1beta1 record looks wrong.
- Comparing pre- vs post-migration to confirm only the expected fields changed.
- Re-creating individual mangled resources by hand without restoring the whole namespace.

See [Recovery from backup](#recovery-from-backup) in the Troubleshooting section if you need to roll back.

## Migration flows

Pick one of three flows depending on how you manage installs.

### Flow A — Manual via kubectl (default)

The official agent-sandbox installation path is `kubectl apply -f` against the release manifests (see the project README and release notes), so this is the default migration flow. Run the script directly from `dev/tools/migrate.sh` (a thin wrapper around `helm/files/migrate.sh`):

```bash
# 1. Pre-create the shadow pools BEFORE applying the new CRDs.
#    Operates on v1alpha1 - this is the last step that does.
bash dev/tools/migrate.sh --phase=bootstrap

# 2. Install the new controller + CRDs (which include the conversion webhook).
#    The release ships two manifests: manifest.yaml (core controller + base
#    CRDs + webhook Service) and extensions.yaml (the extensions API group
#    CRDs: SandboxClaim, SandboxTemplate, SandboxWarmPool). Apply both.
#    Wait until the controller pod is Ready and the webhook Service has
#    endpoints before proceeding.
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.0/manifest.yaml
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.0/extensions.yaml
kubectl rollout status deploy/agent-sandbox-controller -n agent-sandbox-system
kubectl wait --for=condition=Ready pods -l app=agent-sandbox-controller -n agent-sandbox-system

# 3. Force-rewrite every resource in v1beta1 storage format.
bash dev/tools/migrate.sh --phase=migrate
```

If the cluster is large, scope the rewrite to one namespace at a time:

```bash
bash dev/tools/migrate.sh --phase=migrate --namespace=team-alpha
```

### Flow B — Helm-managed, automatic

For installs managed by the Helm chart, the chart ships two Helm hook Jobs that run the bootstrap and rewrite phases automatically as part of `helm upgrade`:

```bash
helm upgrade agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --reuse-values \
  --set image.tag=<new-version>
```

| Phase | Job name | Hook | What it does |
|---|---|---|---|
| Pre-upgrade | `agent-sandbox-migration-bootstrap` | `pre-upgrade` | Runs `migrate.sh --phase=bootstrap` against the existing v1alpha1 state. |
| Post-upgrade | `agent-sandbox-migration-rewrite` | `post-upgrade` | Runs `migrate.sh --phase=migrate` after the new controller + webhook are running. The Job has a `wait-for-webhook` initContainer that polls the webhook Service endpoints for up to 120s. |

Both Jobs run `helm/files/migrate.sh` from a ConfigMap (defaultMode 0755) mounted in the pod. Helm retries on failure (`backoffLimit: 2`); operators can also `kubectl delete job <name>` and re-run `helm upgrade` to re-trigger.

### Flow C — Helm-managed, manual script (hooks disabled)

If you want Helm to manage the chart but want to run the migration script yourself (e.g., to scope to specific namespaces, dry-run first, or run between specific cluster events), disable the hooks:

```bash
# 1. Pre-create shadow pools while v1alpha1 is still the storage version.
bash dev/tools/migrate.sh --phase=bootstrap --dry-run   # inspect first
bash dev/tools/migrate.sh --phase=bootstrap

# 2. Upgrade the chart WITHOUT the migration Jobs.
helm upgrade agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --reuse-values \
  --set image.tag=<new-version> \
  --set migration.enabled=false

# 3. Wait for the new controller + webhook to be Ready, then rewrite storage.
bash dev/tools/migrate.sh --phase=migrate
```

## Dry-runs

Both phases support `--dry-run`. The script prints what it would do without writing anything:

```bash
bash dev/tools/migrate.sh --phase=bootstrap --dry-run
bash dev/tools/migrate.sh --phase=migrate --dry-run
```

The `bootstrap` dry-run also prints the "operator action required" summary (claims referencing missing specific pools), which is useful to inspect even when you intend to apply.

## After migration completes

### Shadow pools

The bootstrap phase creates one `shadow-pool-<template>` per `(namespace, template)` combination referenced by cold-start v1alpha1 claims. They're marked with two annotations:

- `agents.x-k8s.io/migration-shadow: "true"`
- `agents.x-k8s.io/migration-source-template: <template-name>`

List them:

```bash
kubectl get sandboxwarmpools -A -o json \
  | jq -r '.items[]
      | select(.metadata.annotations["agents.x-k8s.io/migration-shadow"]=="true")
      | "\(.metadata.namespace)/\(.metadata.name) (for template: \(.metadata.annotations["agents.x-k8s.io/migration-source-template"]))"'
```

Do **not** delete these pools while any v1beta1 `SandboxClaim` still references them via `warmPoolRef`. v1alpha1 is removed from the codebase in the release this guide accompanies, so there is no v1alpha1 fallback for claims still pointing at a shadow pool. Once you've manually re-pointed any remaining claims to real warm pools, the shadow pools can be cleaned up.

### Re-pointing warm-started claims

The bootstrap phase intentionally **skips** warm-started v1alpha1 claims (those with `warmpool: ""`/`"none"`/`"default"` AND a bound `Sandbox` whose name differs from the claim's). The webhook redirects those claims' `warmPoolRef` to the pool that produced their current `Sandbox` (via `stripRandomSuffix(sandboxName)`), so they end up pointing at a real, existing pool — no shadow needed.

That said, after migration completes you may want to re-point such claims at a different pool (e.g., consolidate, or move to a shadow). The `warmPoolRef.name` is editable on the v1beta1 claim:

```bash
kubectl patch sandboxclaim <name> -n <ns> --type=merge \
  -p '{"spec":{"warmPoolRef":{"name":"my-preferred-pool"}}}'
```

### Operator-action items from the bootstrap summary

If `bootstrap` printed an `OPERATOR ACTION REQUIRED` section listing claims that reference specific pools which don't currently exist, the conversion webhook will still rewrite those claims to point at those exact (missing) pool names. To make those claims work, either:

1. Create the missing pools manually, OR
2. Re-point the claims to existing pools via the `kubectl patch` above.

## Verifying the migration worked

After the post-upgrade Job completes:

```bash
# Every resource should now have the storage-migrated-at annotation.
# jq handles annotation keys with "." and "/" correctly; kubectl jsonpath
# dot-escaping cannot reliably read keys containing "/".
kubectl get sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools -A -o json \
  | jq -r '.items[]
      | "\(.kind) \(.metadata.namespace)/\(.metadata.name) -> \(.metadata.annotations["agents.x-k8s.io/storage-migrated-at"] // "<missing>")"'
```

To verify the actual etcd storage version, check each CRD's `status.storedVersions`. The kube-apiserver records every version that has ever been used to write any record there; after the rewrite Job touches every resource, you can manually prune `v1alpha1` from the list to confirm nothing v1alpha1 is left:

```bash
for crd in \
    sandboxes.agents.x-k8s.io \
    sandboxclaims.extensions.agents.x-k8s.io \
    sandboxtemplates.extensions.agents.x-k8s.io \
    sandboxwarmpools.extensions.agents.x-k8s.io; do
  printf '%s: ' "${crd}"
  kubectl get crd "${crd}" -o jsonpath='{.status.storedVersions}'
  printf '\n'
done
```

If a CRD still lists `["v1alpha1","v1beta1"]` after the rewrite Job succeeded, every existing record has been rewritten in v1beta1 form, but the `storedVersions` array is not auto-pruned. To finalize:

```bash
# Confirm no v1alpha1-only records remain, then prune storedVersions.
kubectl patch crd <crd-name> --subresource=status --type=merge \
  -p '{"status":{"storedVersions":["v1beta1"]}}'
```

Only do this after you've confirmed every existing record carries `agents.x-k8s.io/storage-migrated-at` from the rewrite Job's run.

## Troubleshooting

**Pre-upgrade Job fails with "failed to list SandboxClaims"**: the script's ServiceAccount RBAC isn't applied yet. The bootstrap ClusterRole + ClusterRoleBinding should land in the same Helm hook batch. If you're testing manually, apply `helm/templates/migration-rbac.yaml` first.

**Post-upgrade Job's init container `wait-for-webhook` times out**: the conversion webhook isn't reachable. Check that the Service named by `migration.webhookServiceName` (default `agent-sandbox-webhook-service`) exists in the controller's namespace and that its endpoints are populated:

```bash
kubectl get endpoints agent-sandbox-webhook-service -n agent-sandbox-system
```

**Migrate phase reports failures on specific resources**: re-run the script (`bash dev/tools/migrate.sh --phase=migrate`). It's idempotent — already-migrated resources just get the annotation timestamp updated. If a specific resource keeps failing, fetch it (`kubectl get -o yaml`) and inspect what's wrong — usually it's a conversion-webhook error tied to a bad field combination that needs manual cleanup.

**Bootstrap printed `OPERATOR ACTION REQUIRED` for some claims**: those claims reference specific pool names that don't currently exist. The conversion webhook will still rewrite them to point at those names — you must create the pools manually post-migration, or re-point the claims (see "Re-pointing warm-started claims" above).

### Recovery from backup

If migration produces broken or unexpected v1beta1 resources, use the backup file from [Before you start: back up your data](#before-you-start-back-up-your-data) to restore.

**Per-resource restore** (preferred — only touches what's actually broken):

```bash
# Inspect a specific resource against the backup to confirm it's wrong.
kubectl get <kind> <name> -n <namespace> -o yaml \
  | diff - <(yq '.items[] | select(.kind=="<kind>" and .metadata.name=="<name>")' backup.yaml)

# Delete the broken record and re-apply the v1alpha1 spec from the backup.
# The conversion webhook re-converts it on apply.
kubectl delete <kind> <name> -n <namespace>
yq '.items[] | select(.kind=="<kind>" and .metadata.name=="<name>")' backup.yaml \
  | kubectl apply -f -
```

**Bulk restore** (last resort — only when many resources are broken AND the conversion webhook is functioning):

```bash
# CAUTION: deletes every Sandbox/SandboxClaim/SandboxTemplate/SandboxWarmPool
# across all namespaces, then re-creates them from the backup.
kubectl delete sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools -A --all
kubectl apply -f backup.yaml
```

Caveats:

- Restoration depends on a functioning conversion webhook. If the webhook itself is broken, fix that first (typically: roll the controller image back to the pre-migration version, then re-apply the backup), or restore in two phases by first re-installing the old chart and then re-applying the backup against the old CRDs.
- The backup captures `status` subresources too. Strip them before re-apply so the controllers re-derive status from spec rather than racing your stale snapshot: `yq 'del(.items[].status)' backup.yaml | kubectl apply -f -`.
- Backups don't capture cluster-scoped state like `SandboxWarmPool` controller progress; freshly-applied pools will repopulate themselves from the template.
