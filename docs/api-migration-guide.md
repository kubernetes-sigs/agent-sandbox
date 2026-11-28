# v1alpha1 → v1beta1 API migration guide

The `v1alpha1` API was removed in `v1.0.0`.

If your cluster still has `v1alpha1`-serialized resources in etcd or if you are upgrading from `v0.4.x` / early `v0.5.x` releases, you cannot upgrade directly to `v1.0.0` or later.

## Migration Steps

1. **Upgrade to `v0.5.x` first**: Upgrade your installation to a `v0.5.x` release.
2. **Run the v0.5 migration**: Follow the [v0.5.x API migration guide](https://github.com/kubernetes-sigs/agent-sandbox/blob/v0.5.2/docs/api-migration-guide.md) to migrate all existing resources to `v1beta1` and prune legacy `storedVersions`.
3. **Rewrite `v1alpha1` owner references**: The storage migration rewrites each resource, but not the `ownerReferences` that other objects hold to it. Pods, Services, PersistentVolumeClaims, and NetworkPolicies that the controller created on `v0.4.x` still name their owning `Sandbox` or `SandboxTemplate` with `apiVersion: agents.x-k8s.io/v1alpha1` or `extensions.agents.x-k8s.io/v1alpha1`. So do `Sandbox` objects that a `SandboxClaim` adopted. After the upgrade, the Kubernetes garbage collector cannot resolve a `v1alpha1` owner (see [kubernetes/kubernetes#102641](https://github.com/kubernetes/kubernetes/issues/102641)). Deleting a pre-upgrade `SandboxClaim` with `--cascade=foreground` then leaves it in `foregroundDeletion`, and its `Sandbox` and Pod are never collected. With the default background deletion, the claim may disappear while its Sandbox and Pod remain orphaned. Automatic rewriting is proposed in [#2129](https://github.com/kubernetes-sigs/agent-sandbox/pull/2129); do not assume that a `v1.0.x` release includes it. Unless the target release explicitly documents that it refreshes these references, the manual rewrite below is **required** while `v0.5.x` is still running. The `test` operation makes each patch fail rather than touch a reference that changed in the meantime:
   ```bash
   set -euo pipefail
   STALE='^(extensions\.)?agents\.x-k8s\.io/v1alpha1$'
   for kind in pods services persistentvolumeclaims networkpolicies \
       sandboxes.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io \
       sandboxwarmpools.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io; do
     kubectl get "${kind}" -A -o json | jq -r --arg stale "${STALE}" '
       .items[]
       | [.metadata.namespace, .metadata.name,
          ([.metadata.ownerReferences // [] | to_entries[] | select(.value.apiVersion | test($stale))
            | {op: "test", path: "/metadata/ownerReferences/\(.key)/uid", value: .value.uid},
              {op: "replace", path: "/metadata/ownerReferences/\(.key)/apiVersion",
               value: (.value.apiVersion | sub("v1alpha1$"; "v1beta1"))}] | tojson)]
       | select(.[2] != "[]") | @tsv' |
     while IFS=$'\t' read -r namespace name patch; do
       kubectl patch "${kind}" "${name}" -n "${namespace}" --type=json -p "${patch}"
     done
   done
   ```
4. **Pre-upgrade check**: Before upgrading from `v0.5.x` to `v1.0.0`, verify that every agent-sandbox CustomResourceDefinition reports only `v1beta1` in `status.storedVersions`:
   ```bash
   set -euo pipefail
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

   After the manual rewrite in step 3, also verify that no object still names a `v1alpha1` owner. The following command prints nothing when the rewrite is complete:
   ```bash
   set -euo pipefail
   for kind in pods services persistentvolumeclaims networkpolicies \
       sandboxes.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io \
       sandboxwarmpools.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io; do
     kubectl get "${kind}" -A -o json | jq -r --arg kind "${kind}" \
       '.items[] | select(any(.metadata.ownerReferences[]?; .apiVersion | test("^(extensions\\.)?agents\\.x-k8s\\.io/v1alpha1$")))
        | "\($kind) \(.metadata.namespace)/\(.metadata.name)"'
   done
   ```
5. **Replace the CRDs, then upgrade the controller**: Once all four CRDs report only `["v1beta1"]` and the owner-reference check prints nothing, install the target release's CRDs **while the `v0.5.x` controller and conversion webhook are still running**. Replace only CRDs before updating any controller or webhook resources.

   This ordering applies to both Helm and release-manifest installations. In particular, server-side apply (`kubectl apply --server-side`) of the target CRDs can fail even after storage migration: the `v0.5.x` controller owns the conversion webhook's injected `caBundle`, so your applying field manager does not remove it. That leaves a `caBundle` without `strategy: Webhook`, which the API server rejects. The v0.5.2 Helm migration guide also used server-side apply, so do not assume a Helm installation is unaffected. `kubectl replace` replaces each CRD's complete spec, including removal of the old conversion configuration.

   **Helm:** From a checkout of the target release, replace the CRDs first, then run your usual `helm upgrade`:
   ```bash
   kubectl replace -f helm/crds/
   # Now run helm upgrade with your existing release name and values.
   ```

   **Release manifests:** Download the target release manifest and extract only its CRDs (requires [Mike Farah's yq v4](https://mikefarah.gitbook.io/yq/)). Choose the same manifest variant as your installation, for example `sandbox-with-extensions.yaml`:
   ```bash
   set -euo pipefail
   VERSION=v1.0.0  # Set this to your target release.
   MANIFEST=sandbox-with-extensions.yaml
   curl --fail --location \
     "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/${MANIFEST}" \
     --output agent-sandbox-v1.yaml
   yq eval 'select(.kind == "CustomResourceDefinition")' agent-sandbox-v1.yaml \
     > agent-sandbox-v1-crds.yaml
   test -s agent-sandbox-v1-crds.yaml
   kubectl replace -f agent-sandbox-v1-crds.yaml
   # Only after CRD replacement succeeds, update the remaining resources.
   kubectl apply -f agent-sandbox-v1.yaml
   ```
   Do **not** run `kubectl replace` against the complete release manifest: it can replace the controller before the CRDs. This procedure completes the manual owner-reference rewrite before the upgrade, even if the target release also supports automatic refresh.
6. **Restart kube-controller-manager**: The garbage collector remembers the API version it first saw for each object. Objects that it saw as `v1alpha1` stay `v1alpha1` in its dependency graph, even after their owner references are rewritten. Once `v1.0.0` stops serving `v1alpha1`, every deletion that involves such an object fails and retries indefinitely. kube-controller-manager logs `"error syncing item" err="unable to get REST mapping for agents.x-k8s.io/v1alpha1/Sandbox."` for each one. This also blocks new `SandboxClaims` that adopt a warm-pool `Sandbox` created before the upgrade. Restart kube-controller-manager after the owner references are rewritten, so that it rebuilds the graph from the served version. A restart before the rewrite finishes rebuilds the graph from the stale references. How you restart it depends on how your control plane runs. For example, on kind, stop the static Pod's container and let the kubelet start a new one:
   ```bash
   docker exec kind-control-plane sh -c 'crictl stop $(crictl ps -q --name kube-controller-manager)'
   ```
   On OpenShift, set `spec.forceRedeploymentReason` on the `kubecontrollermanager/cluster` resource. Managed control planes may need a request to the provider.
7. **Verify**: Select a **disposable** `SandboxClaim` that existed before the upgrade and whose Sandbox can safely be deleted. Back up or otherwise protect any required data first: cascading deletion can remove owned PVCs and their data, depending on the storage reclaim policy. Delete this claim using foreground deletion so that unresolved dependents keep the claim visible:
   ```bash
   kubectl delete sandboxclaim <name> -n <namespace> --cascade=foreground --wait=false
   ```
   The claim, its `Sandbox`, and its Pod should be gone within a couple of minutes. If the claim stays in `foregroundDeletion`, check for `v1alpha1` owner references again (step 4) and repeat step 6. A claim disappearing under default background deletion is not proof that its dependents were collected.

## Helm Upgrade Ordering

Helm does not upgrade CRDs in `crds/` (see [helm/README.md](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/helm/README.md)), so the order matters. Apply the CRDs **before** `helm upgrade`, otherwise the chart removes the webhook Service while the old CRDs still reference it for conversion.

Follow Migration Step 5 above: replace the CRDs from the target release with `kubectl replace -f helm/crds/` while the old conversion webhook is still running, then run `helm upgrade`. After the owner-reference and garbage-collector steps, delete the orphaned cert Secret as described below. This ordering also covers CRDs previously installed with server-side apply.

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
