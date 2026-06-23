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

The migration script executes in two distinct phases. **Neither phase can happen in an arbitrary order**, and if you have existing cold-start claims, skipping the bootstrap phase will immediately break them upon upgrading to `v0.5.0`.

### Phase 1: `--phase=bootstrap` (Conditionally Mandatory, Pre-Upgrade)

- **Mandatory for `v0.5.0`:** Yes, if you have existing cold-start `v1alpha1` claims (claims where `spec.warmpool` is empty/`"none"`/`"default"` AND `status.sandbox.name` matches the claim name or is empty). Note: The script automatically detects whether cold-start claims exist. If none exist, it safely exits without creating anything, so it is recommended to always run it (or use `--dry-run` to inspect) rather than skipping it manually.
- **Timing:** Strictly **before** upgrading to `v0.5.0` (while `v1alpha1` is still active).
- **What it does:** Scans existing `v1alpha1` claims and pre-creates `shadow-pool-<template>` warm pools. The `v0.5.0` controller reconciler is written purely against `v1beta1.SandboxClaim`, which requires a valid `spec.warmPoolRef.name`. If you do not pre-create the shadow pools, the conversion webhook will point converted claims to non-existent infrastructure, leaving converted claims stuck with a `WarmPoolNotFound` condition while the controller repeatedly requeues them.
- **Why it cannot run after upgrade:** Bootstrap relies on `v1alpha1` field inspection. Once `v0.5.0` is installed, `kubectl get sandboxclaims` defaults to returning `v1beta1` objects (which lack `spec.sandboxTemplateRef`). The script will see empty template names, log errors for every claim, and fail to create any shadow pools.

### Phase 2: `--phase=migrate` (Optional for `v0.5.0`, Post-Upgrade)

- **Optional for `v0.5.0`** (but mandatory before upgrading to a future release that drops `v1alpha1`).
- **Timing:** Strictly **after** upgrading to `v0.5.0` (when `v1beta1` is established as the storage version and the conversion webhook is live).
- **What it does:** Patches every existing resource with a benign annotation (`agents.x-k8s.io/storage-migrated-at`). This forces the API server to read the `v1alpha1` etcd record, pass it through the conversion webhook, and rewrite it to etcd in `v1beta1` storage format. While the kube-apiserver can translate older records on the fly for `v0.5.0`, running this phase ensures all objects are re-serialized in `v1beta1` format in etcd, which is required before `v1alpha1` can be safely removed from the CRD definition in a future release.
- **Why it cannot run before upgrade:** Before upgrading, `v1alpha1` is still the storage version in etcd. Patching the resources will merely write an annotation onto `v1alpha1` etcd records, accomplishing zero storage migration.

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

# Wait until the conversion webhook is responsive (this may take a few seconds after the pod starts)
until kubectl get sandboxwarmpools.extensions.agents.x-k8s.io -A >/dev/null 2>&1; do
  echo "Waiting for conversion webhook to be responsive..."
  sleep 2
done

# 3. Force-rewrite every resource in v1beta1 storage format.
bash dev/tools/migrate.sh --phase=migrate
```

If the cluster is large, scope the rewrite to one namespace at a time:

```bash
bash dev/tools/migrate.sh --phase=migrate --namespace=team-alpha
```

### Flow B — Helm-managed, manual script

For installs managed by the Helm chart, the migration is driven manually by the operator using `dev/tools/migrate.sh`.

```bash
# 1. Pre-create shadow pools while v1alpha1 is still the storage version.
bash dev/tools/migrate.sh --phase=bootstrap --dry-run   # inspect first
bash dev/tools/migrate.sh --phase=bootstrap

# 2. Manually apply the upgraded CRD manifests using Server-Side Apply.
#    Since Helm does not upgrade CRDs on upgrade, they must be applied manually:
kubectl apply --server-side --force-conflicts -f path/to/chart/crds/

# 3. Upgrade the chart.
# (If you are using extension resources like claims, templates, or pools, make sure --set controller.extensions=true is set or enabled in values)
helm upgrade agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --reuse-values \
  --set image.tag=<new-version> \
  --set controller.extensions=true

# 4. Wait for the new controller + webhook to be Ready, then rewrite storage.
kubectl rollout status deploy/agent-sandbox-controller -n agent-sandbox-system

# Wait until the conversion webhook is responsive (this may take a few seconds after the pod starts)
until kubectl get sandboxwarmpools.extensions.agents.x-k8s.io -A >/dev/null 2>&1; do
  echo "Waiting for conversion webhook to be responsive..."
  sleep 2
done
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

Do **not** delete these pools while any v1beta1 `SandboxClaim` still references them via `warmPoolRef`. While `v1alpha1` definitions remain in the codebase for webhook conversion, the `v1beta1` controller reconciler has no `v1alpha1` fallback logic for claims pointing to missing pools. Once you've manually re-pointed any remaining claims to real warm pools, the shadow pools can be cleaned up.

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

**Migrate phase reports failures on specific resources**: re-run the script (`bash dev/tools/migrate.sh --phase=migrate`). It's idempotent — already-migrated resources just get the annotation timestamp updated. If a specific resource keeps failing, fetch it (`kubectl get -o yaml`) and inspect what's wrong — usually it's a conversion-webhook error tied to a bad field combination that needs manual cleanup.

**Bootstrap printed `OPERATOR ACTION REQUIRED` for some claims**: those claims reference specific pool names that don't currently exist. The conversion webhook will still rewrite them to point at those names — you must create the pools manually post-migration, or re-point the claims (see "Re-pointing warm-started claims" above).

**Webhook connection timeouts in managed/private clusters (e.g., GKE)**: If you see `dial tcp ... connect: connection refused` or connection timeouts from the API server during the `migrate` phase, it is likely that the control plane VPC cannot reach the webhook target port (`9443`) on the worker nodes.
* By default, GKE private clusters block master-to-worker node traffic on ports other than standard ones like `443` and `10250`.
* **Fix**: Create a firewall rule in your GCP console allowing ingress from your GKE master node IP range to your worker nodes on TCP port `9443`.


## Emergency Rollback Procedure (Reverting to v1alpha1)

If the migration fails critically (e.g., the new controller fails to start, the webhook causes severe issues, or you encounter unresolvable errors) and you need to completely revert to the `v1alpha1` version:

### Step 1: Disable Conversion Webhooks
First, stop the API server from attempting version conversion to prevent blockages on custom resource writes and deletions:
```bash
for crd in \
    sandboxes.agents.x-k8s.io \
    sandboxclaims.extensions.agents.x-k8s.io \
    sandboxtemplates.extensions.agents.x-k8s.io \
    sandboxwarmpools.extensions.agents.x-k8s.io; do
  kubectl patch crd "${crd}" --type=merge -p '{"spec":{"conversion":{"strategy":"None","webhook":null}}}'
done
```
### Step 2: Scale down the controller deployment
Scale down the `agent-sandbox-controller` deployment to `0` replicas, and wait for the pods to terminate completely. This stops the controller manager from reconciling resources or creating new Sandboxes to replace deleted ones while you are cleaning up the resources:
```bash
kubectl scale deploy/agent-sandbox-controller -n agent-sandbox-system --replicas=0
kubectl wait --for=delete pod -l app=agent-sandbox-controller -n agent-sandbox-system --timeout=60s
```

### Step 3: Delete upgraded resources
While the upgraded CRDs (supporting both `v1alpha1` and `v1beta1` versions) are still installed, delete all custom resources so etcd is completely emptied of `v1beta1` records:
```bash
kubectl delete sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools -A --all
```

### Step 4: Delete shadow pools (optional)
If the bootstrap phase created shadow warm pools, delete them:
```bash
kubectl get sandboxwarmpools -A -o json \
  | jq -r '.items[] | select(.metadata.annotations["agents.x-k8s.io/migration-shadow"]=="true") | "\(.metadata.namespace)/\(.metadata.name)"' \
  | xargs -I {} sh -c 'kubectl delete sandboxwarmpool $(echo {} | cut -d/ -f2) -n $(echo {} | cut -d/ -f1)'
```

### Step 5: Reset CRD storedVersions to v1alpha1
Because the API server enforces that any version in `status.storedVersions` must be present in `spec.versions`, you must patch the CRDs to list only `v1alpha1` in their stored versions before downgrading the CRD definitions:
```bash
for crd in \
    sandboxes.agents.x-k8s.io \
    sandboxclaims.extensions.agents.x-k8s.io \
    sandboxtemplates.extensions.agents.x-k8s.io \
    sandboxwarmpools.extensions.agents.x-k8s.io; do
  kubectl patch crd "${crd}" --subresource=status --type=merge -p '{"status":{"storedVersions":["v1alpha1"]}}'
done
```

### Step 6: Revert the CRD manifests and Controller
Downgrade the installed components back to the old version:

* **For Flow A (kubectl):** Re-apply the old version's manifests (substitute `<old-version>` with your previous version, e.g., `v0.4.6`):
  ```bash
  kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/<old-version>/manifest.yaml
  kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/<old-version>/extensions.yaml
  ```
* **For Flow B (Helm):** Roll back the Helm release to the pre-migration revision (find the revision number using `helm history agent-sandbox`):
  ```bash
  helm rollback agent-sandbox <previous-revision-number> -n agent-sandbox-system
  # Re-apply the old CRD versions manually:
  kubectl apply --server-side --force-conflicts -f path/to/old-chart/crds/
  ```

### Step 7: Restore Data from Backup
Apply the backup file to restore your original `v1alpha1` resources:
```bash
# Re-apply the backup (stripping status fields is recommended to allow the old controller to re-initialize them)
yq 'del(.items[].status)' backup.yaml | kubectl apply -f -
```

---

## Recovery from Backup (Remaining on v1beta1)

If you intend to stay on `v1beta1` but need to restore specific broken or corrupt objects from your backup:

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
