# Configuration

The `agent-sandbox-controller` supports several command-line flags to tune performance and scalability under high load or in large clusters.

## Concurrency Settings

* `--sandbox-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the Sandbox controller.
* `--sandbox-claim-concurrent-workers` (default: 50): The maximum number of concurrent reconciles for the SandboxClaim controller.
* `--sandbox-warm-pool-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxWarmPool controller.
* `--sandbox-warm-pool-max-batch-size` (default: 300): The maximum number of sandboxes the SandboxWarmPool controller will create/delete in a single batch.
* `--kube-api-qps` (default: -1, no client-side rate limiting): Client-side QPS limit for the Kubernetes API client.
* `--kube-api-burst` (default: 10): The maximum burst for client-side throttling of the Kubernetes API client.

## Namespace Scoping

By default the controller watches all namespaces. Use `--namespace` (or the `WATCH_NAMESPACE` environment variable) to restrict it to one or more namespaces.

* `--namespace` (default: `""`, cluster-scoped): Comma-separated list of namespaces to watch. When set, the controller only caches and reconciles resources in those namespaces. Falls back to the `WATCH_NAMESPACE` environment variable when the flag is not provided.

### Single-namespace mode

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller
        args:
        - --leader-elect=true
        - --namespace=my-team-ns
```

When a single namespace is given and `--leader-election-namespace` is not set, the leader election Lease is automatically created in the same namespace, keeping RBAC fully scoped to that namespace.

### Multi-namespace mode

```yaml
        args:
        - --leader-elect=true
        - --namespace=team-a,team-b,team-c
        - --leader-election-namespace=agent-sandbox-system
```

For multi-namespace deployments you must set `--leader-election-namespace` explicitly, as the controller cannot pick an unambiguous namespace for the lease.

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

If using the core controller, update `manifest.yaml`:

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
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-max-batch-size=500"},
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
