# v1alpha1 → v1beta1 API migration guide

This document covers the operational side of migrating `Sandbox`, `SandboxClaim`, `SandboxTemplate`, and `SandboxWarmPool` resources from the `v1alpha1` API to the `v1beta1` API.

If you install the chart fresh with the v1beta1-storage version, there is nothing to migrate — read this only when **upgrading** an existing installation that holds v1alpha1-serialized resources in etcd.

## What changes between versions

Most CRDs are schema-compatible across the two versions; the migration matters mainly for **two reasons**:

1. **`SandboxClaim` is not field-compatible.** v1alpha1 has `spec.sandboxTemplateRef` plus an optional `spec.warmpool` string policy (`"none"` / `"default"` / a specific pool name). v1beta1 requires `spec.warmPoolRef.name`. The conversion webhook handles the field rename automatically, but claims that used `warmpool: "none" / "default" / unset` have no obvious target pool — bootstrap creates per-claim shadow pools to fill the gap.
2. **`Sandbox.spec.replicas` becomes `Sandbox.spec.operatingMode`.** `replicas: 0` → `Suspended`, `replicas: 1` (or unset) → `Running`. The webhook handles this automatically.

The other two CRDs (`SandboxTemplate`, `SandboxWarmPool`) are structurally identical between versions but still need a storage rewrite so etcd holds them in v1beta1 form.

## What runs automatically

`helm upgrade agent-sandbox ./helm/ ...` triggers two Jobs via Helm hook annotations:

| Phase | Job name | Hook | Does what |
|---|---|---|---|
| Pre-upgrade | `agent-sandbox-migration-bootstrap` | `pre-upgrade` | Scans every existing `SandboxClaim`. For each one whose `spec.warmpool` is `"none"`, `"default"`, unset, or names a pool that doesn't exist, creates a shadow `SandboxWarmPool` named `<claim>-shadow-pool` (replicas=0, references the claim's existing template). This gives the conversion webhook a valid `warmPoolRef` target. |
| Post-upgrade | `agent-sandbox-migration-rewrite` | `post-upgrade` | Patches every existing `Sandbox`, `SandboxClaim`, `SandboxTemplate`, and `SandboxWarmPool` with a `agents.x-k8s.io/storage-migrated-at` annotation. The patch round-trips the resource through the conversion webhook, which causes the API server to rewrite the etcd record in v1beta1 format. |

Both Jobs are **idempotent**. Helm will retry on failure (`backoffLimit: 2`); operators can also `kubectl delete job <name>` and `helm upgrade --no-hooks=false` to re-trigger.

## What runs manually

The same script the Jobs use is exposed at `dev/tools/migrate.sh` for operators who want finer control:

```bash
# Dry-run the bootstrap phase against the cluster your kubeconfig points at.
bash dev/tools/migrate.sh --phase=bootstrap --dry-run

# Actually create shadow pools.
bash dev/tools/migrate.sh --phase=bootstrap

# Trigger the storage rewrite for everything.
bash dev/tools/migrate.sh --phase=migrate

# Scope to one namespace.
bash dev/tools/migrate.sh --phase=migrate --namespace=team-alpha
```

The script uses `kubectl` and respects the active kubeconfig. It exits non-zero if any resource fails to migrate (per-resource errors don't abort the run, but the final exit reflects total failures).

## Opting out of the automated Jobs

```bash
helm upgrade agent-sandbox ./helm/ ... --set migration.enabled=false
```

Then run `dev/tools/migrate.sh` manually in whatever order and scope you want. Useful when:

- The cluster is large enough that the default Job resource limits aren't appropriate (override `--set migration.resources....` or run manually).
- You want to migrate one namespace at a time as part of a phased rollout.
- The cluster has CRDs from a custom build of agent-sandbox and you need to validate behavior before letting the automated Job touch everything.

## After migration completes

The shadow `SandboxWarmPool`s created by the bootstrap phase remain in the cluster — the conversion webhook depends on them being valid `warmPoolRef` targets for the converted v1beta1 `SandboxClaim`s. They are marked with two annotations so you can find them later:

```bash
kubectl get sandboxwarmpools -A -o json \
  | jq -r '.items[]
      | select(.metadata.annotations["agents.x-k8s.io/migration-shadow"]=="true")
      | "\(.metadata.namespace)/\(.metadata.name) (for claim: \(.metadata.annotations["agents.x-k8s.io/migration-source-claim"]))"'
```

Do **not** delete these pools while the corresponding `SandboxClaim`s still reference them via `warmPoolRef`. Once v1alpha1 is fully removed from the codebase (a future release) and you've manually re-pointed any remaining claims to real warm pools, the shadow pools can be cleaned up.

## Verifying the migration worked

After the post-upgrade Job completes:

```bash
# Every resource should now have the storage-migrated-at annotation.
kubectl get sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools -A \
  -o jsonpath='{range .items[*]}{.kind}{" "}{.metadata.namespace}/{.metadata.name}{" -> "}{.metadata.annotations.agents\.x-k8s\.io/storage-migrated-at}{"\n"}{end}'

# Check the actual etcd storage version (requires cluster-admin):
kubectl get --raw '/apis/extensions.agents.x-k8s.io/v1beta1/sandboxclaims' \
  | jq '.items[0].apiVersion'   # expect "extensions.agents.x-k8s.io/v1beta1"
```

## Troubleshooting

**Pre-upgrade Job fails with "failed to list SandboxClaims"**: the script's ServiceAccount RBAC isn't applied yet. The bootstrap ClusterRole + ClusterRoleBinding should land in the same Helm hook batch. If you're testing manually, apply `helm/templates/migration-rbac.yaml` first.

**Post-upgrade Job's init container `wait-for-webhook` times out**: the conversion webhook isn't reachable. Check that the `agent-sandbox-webhook-service` Service exists in the controller's namespace and that its endpoints are populated (`kubectl get endpoints agent-sandbox-webhook-service -n agent-sandbox-system`). If the Service is missing entirely, that's a chart issue separate from this migration — track it upstream.

**Migrate phase reports failures on specific resources**: re-run the script (`bash dev/tools/migrate.sh --phase=migrate`). It's idempotent — already-migrated resources just get the annotation timestamp updated. If a specific resource keeps failing, fetch it (`kubectl get -o yaml`) and inspect what's wrong — usually it's a conversion-webhook error tied to a bad field combination that needs manual cleanup.
