# v1alpha1 → v1beta1 API migration guide

The `v1alpha1` API was removed in `v1.0.0`.

If your cluster still has `v1alpha1`-serialized resources in etcd or if you are upgrading from `v0.4.x` / early `v0.5.x` releases, you cannot upgrade directly to `v1.0.0` or later.

## Migration Steps

1. **Upgrade to `v0.5.x` first**: Upgrade your installation to a `v0.5.x` release.
2. **Run the v0.5 migration**: Follow the [v0.5.x API migration guide](https://github.com/kubernetes-sigs/agent-sandbox/blob/v0.5.2/docs/api-migration-guide.md) to migrate all existing resources to `v1beta1` and prune legacy `storedVersions`.
3. **Pre-upgrade check**: Before upgrading from `v0.5.x` to `v1.0.0`, verify that every agent-sandbox CustomResourceDefinition reports only `v1beta1` in `status.storedVersions`:
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
   If `v1alpha1` is still listed on any CRD, complete the v0.5 storage migration and CRD status prune first—otherwise the Kubernetes apiserver will reject the upgrade.
4. **Upgrade to `v1.0.0`**: Once all four CRDs report only `["v1beta1"]`, proceed with upgrading to `v1.0.0` or later.

## Helm Upgrade Ordering

Helm does not upgrade CRDs in `crds/` (see [helm/README.md](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/helm/README.md)), so the order matters. Apply the CRDs **before** `helm upgrade`, otherwise the chart removes the webhook Service while the old CRDs still reference it for conversion.

After completing Migration Steps 1–3 above:

1. `kubectl apply -f helm/crds/` — installs the v1beta1-only CRDs and drops the conversion config.
2. `helm upgrade` — now safe; removes the webhook Service, Role, and RoleBinding.
3. Delete the orphaned cert Secret (see Post-Upgrade Cleanup below).

## Post-Upgrade Cleanup

When upgrading via `kubectl apply -f k8s/` or sequential manifest application, Kubernetes does not automatically delete resources that have been removed from the newest release manifests. After upgrading to `v1.0.0`, four legacy webhook infrastructure objects from `v0.5.x` will remain in your cluster as orphans:
- `Service/agent-sandbox-webhook-service`
- `Secret/agent-sandbox-webhook-certs`
- `Role/agent-sandbox-controller` (namespaced in `agent-sandbox-system`)
- `RoleBinding/agent-sandbox-controller` (namespaced in `agent-sandbox-system`)

To clean up these orphaned webhook resources, run:
```bash
kubectl delete -n agent-sandbox-system \
  svc/agent-sandbox-webhook-service \
  secret/agent-sandbox-webhook-certs \
  role/agent-sandbox-controller \
  rolebinding/agent-sandbox-controller \
  --ignore-not-found
```

> [!WARNING]
> The cluster-scoped `ClusterRole` and `ClusterRoleBinding` named `agent-sandbox-controller` are still actively required by the running controller. Do **not** delete cluster-scoped roles; ensure the cleanup snippet above uses namespaced `role`/`rolebinding` within `-n agent-sandbox-system`.

> [!NOTE]
> **Helm and OLM Users**:
> - **Helm**: `helm upgrade` automatically prunes `Service/agent-sandbox-webhook-service`, `Role/agent-sandbox-controller`, and `RoleBinding/agent-sandbox-controller`. Helm users only need to delete `Secret/agent-sandbox-webhook-certs`.
> - **OLM**: OLM automatically garbage-collects `Role/agent-sandbox-controller` and `RoleBinding/agent-sandbox-controller` on CSV replacement. However, both `Service/agent-sandbox-webhook-service` and `Secret/agent-sandbox-webhook-certs` remain and must be deleted manually.
> - **All users** (kubectl, Helm, OLM) can safely run the 4-resource `kubectl delete` cleanup command above with `--ignore-not-found`. (See [OLM Installation Guide](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/olm/README.md#upgrading-from-v05x-to-v100) for OLM-specific instructions).
