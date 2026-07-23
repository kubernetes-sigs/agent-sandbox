# Configuration

The `agent-sandbox-controller` supports several command-line flags to tune performance and scalability under high load or in large clusters.

## Concurrency Settings

* `--sandbox-concurrent-workers` (default: 100): The maximum number of concurrent reconciles for the Sandbox controller.
* `--sandbox-claim-concurrent-workers` (default: 50): The maximum number of concurrent reconciles for the SandboxClaim controller.
* `--sandbox-warm-pool-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxWarmPool controller.
* `--sandbox-warm-pool-max-batch-size` (default: 300): The maximum number of sandboxes the SandboxWarmPool controller will create/delete in a single batch.
* `--kube-api-qps` (default: -1, no client-side rate limiting): Client-side QPS limit for the Kubernetes API client.
* `--kube-api-burst` (default: 10): The maximum burst for client-side throttling of the Kubernetes API client.

## Namespace Scoping

By default the controller watches all namespaces. Use `--namespace` (or the `WATCH_NAMESPACE` environment variable) to restrict it to one or more namespaces.

* `--namespace` (default: `""`, cluster-scoped): Comma-separated list of namespaces to watch. When set, the controller only caches and reconciles resources in those namespaces. Falls back to the `WATCH_NAMESPACE` environment variable when the flag is not provided. Requires `--enable-webhook=false`.

> **Conversion webhooks are disabled in namespaced mode.** Webhook certificate generation, CRD CA-bundle patching, and the conversion webhook server are all skipped when `--namespace` (or `WATCH_NAMESPACE`) is set. These operate on cluster-scoped resources (`CustomResourceDefinition`s and their conversion webhooks), which a namespace-scoped deployment cannot manage. As a result the controller needs **no cluster-scoped RBAC** and can run with only a `Role`/`RoleBinding`. The CRDs and their conversion webhooks must instead be installed and managed cluster-wide — by a cluster admin or a separate cluster-scoped controller instance.
>
> **The v1alpha1 API is effectively unusable in namespaced mode.** The stock CRDs keep `v1alpha1` with `served: true` and `conversion.strategy: Webhook`. Without a working conversion webhook, *any* request addressed to `v1alpha1` (e.g., `kubectl get sandboxes.v1alpha1.agents.x-k8s.io`, or clients pinned to v1alpha1) fails — even on a fresh cluster with zero stored v1alpha1 objects — because conversion is required whenever the request version differs from the storage version (`v1beta1`). Before deploying in namespaced mode, either complete the [API storage migration](api-migration-guide.md) and remove the served v1alpha1 versions from the CRDs, or keep a cluster-scoped controller or externally managed conversion webhook available.
>
> No admission webhooks (defaulting or validating) are registered by this controller — all field defaults are declared as `+kubebuilder:default` markers in the CRD schema and are applied by the API server directly, so admission behavior is identical in namespaced and cluster-scoped modes.

### Controller argument behavior

The following matrix assumes `--leader-elect=true`. An unset `--enable-webhook`
is equivalent to `--enable-webhook=true`.

| `--enable-webhook` | `--namespace` | Leader namespace explicit | Leader namespace empty, in-cluster | Leader namespace empty, out-of-cluster |
|---|---|---|---|---|
| Unset or `true` | Empty | Starts cluster-scoped; webhooks enabled; Lease in specified namespace | Starts cluster-scoped; webhooks enabled; Lease in Pod namespace | Startup fails: leader-election namespace cannot be determined |
| Unset or `true` | Single | Startup validation error: namespaced mode requires `--enable-webhook=false` | Same validation error | Same validation error |
| Unset or `true` | Multiple | Startup validation error: namespaced mode requires `--enable-webhook=false` | Same validation error | Same validation error |
| `false` | Empty | Starts cluster-scoped; webhooks disabled; Lease in specified namespace | Starts cluster-scoped; webhooks disabled; Lease in Pod namespace | Startup fails: leader-election namespace cannot be determined |
| `false` | Single | Starts single-namespace; webhooks disabled; Lease in specified namespace | Starts single-namespace; webhooks disabled; Lease in Pod namespace | Startup fails: explicit leader-election namespace required |
| `false` | Multiple | Starts multi-namespace; webhooks disabled; Lease in specified namespace | Starts multi-namespace; webhooks disabled; Lease in Pod namespace | Startup fails: explicit leader-election namespace required |

If leader-election RBAC is missing, the process can remain running while the
controllers fail to acquire leadership and therefore do not reconcile.

### Helm behavior

The Helm chart has no first-class `controller.enableWebhook` value. Without
`controller.watchNamespace`, the chart omits the flag and webhooks default to
enabled. With `controller.watchNamespace`, the chart injects
`--enable-webhook=false`. An explicit override can be added through
`controller.extraArgs`.

When `controller.watchNamespace` is set, the chart does not render controller
`ClusterRole` or `ClusterRoleBinding` resources and does not generate workload
or leader-election `Role` or `RoleBinding` resources. Installation NOTES leave
those namespace-scoped permissions to advanced users. They must create workload
permissions in every watched namespace and, when leader election is enabled,
leader-election permissions in the effective leader namespace, all bound to the
controller ServiceAccount.

An explicit `--enable-webhook=true` in `controller.extraArgs` overrides the
chart-provided `false` value and causes startup validation to fail in namespaced
mode. Setting `--enable-webhook=false` without `controller.watchNamespace`
disables webhooks but leaves the controller cluster-scoped.

### Single-namespace mode

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller
        args:
        - --leader-elect=true
        - --namespace=my-team-ns
        - --enable-webhook=false
```

When `--leader-election-namespace` is not set and the controller is running in-cluster, controller-runtime resolves the lease namespace from the pod's own service-account namespace (typically `agent-sandbox-system`). When running out-of-cluster you must set `--leader-election-namespace` explicitly.

By default, the Helm chart injects `--leader-election-namespace=<release-namespace>` in namespaced mode. Advanced users must create the required leader-election Role and RoleBinding in that namespace, or in the namespace selected by `controller.leaderElectionNamespace`.

### Multi-namespace mode

```yaml
        args:
        - --leader-elect=true
        - --namespace=team-a,team-b,team-c
        - --enable-webhook=false
        - --leader-election-namespace=agent-sandbox-system
```

For multi-namespace deployments you must set `--leader-election-namespace` explicitly when running out-of-cluster; in-cluster the pod's own namespace is used.

### Downward API / environment variable

The `WATCH_NAMESPACE` environment variable follows the [Operator SDK convention](https://sdk.operatorframework.io/docs/building-operators/golang/operator-scope/) and is useful for Helm or OLM deployments where the watched namespace is the pod's own namespace:

```yaml
        env:
        - name: WATCH_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
```

## Cluster Settings

* `--cluster-domain` (default: `cluster.local`): The Kubernetes cluster domain used to
  construct service FQDNs. Only change this if your cluster is configured with a non-default
  domain (e.g. `my-company.local`).

## Deployment Example

To deploy the controller with custom concurrency settings, modify the `args` of the `agent-sandbox-controller` container within the project's installation manifests. 

If using the core controller, update `sandbox.yaml`:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --sandbox-concurrent-workers=10
```

If you are deploying the extensions controller (which includes the core controllers + extensions), update the args in `extensions.yaml` instead:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --extensions
        - --sandbox-concurrent-workers=10
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-warm-pool-concurrent-workers=10
        - --sandbox-warm-pool-max-batch-size=500
```
**Using `kubectl patch` (Live Cluster):**
If you have already deployed the controller (e.g., via `make deploy-kind`) and want to apply these concurrency flags dynamically to the running cluster, you can use a JSON patch:

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type='json' \
  -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-claim-concurrent-workers=100"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-max-batch-size=500"}
  ]'
```
This method safely appends the new flags without overwriting existing necessary arguments like `--leader-elect=true` or `--extensions=true`.

**Using Kustomize:**
If you prefer applying patches via Kustomize rather than modifying the base manifests directly, you can create a patch file (e.g., `patch-args.yaml`):

```yaml
# patch-args.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-sandbox-controller
  namespace: agent-sandbox-system
spec:
  template:
    spec:
      containers:
      - name: agent-sandbox-controller
        args:
        - --sandbox-concurrent-workers=10
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-warm-pool-concurrent-workers=10
        - --sandbox-warm-pool-max-batch-size=500
```
Then include the patch in your `kustomization.yaml`:
```yaml
patches:
  - path: patch-args.yaml
```
