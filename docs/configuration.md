# Configuration

All controller configuration is exposed as named values in the Helm chart's `values.yaml`. The full list of parameters and their defaults is in the [Helm chart README](../helm/README.md).

## Concurrency Settings

| Value | Default | Description |
|-------|---------|-------------|
| `controller.sandboxConcurrentWorkers` | `1` | Max concurrent reconciles for the Sandbox controller |
| `controller.sandboxClaimConcurrentWorkers` | `1` | Max concurrent reconciles for the SandboxClaim controller |
| `controller.sandboxWarmPoolConcurrentWorkers` | `1` | Max concurrent reconciles for the SandboxWarmPool controller |
| `controller.sandboxTemplateConcurrentWorkers` | `1` | Max concurrent reconciles for the SandboxTemplate controller |
| `controller.kubeApiQps` | `-1` (unlimited) | QPS limit for the Kubernetes API client |
| `controller.kubeApiBurst` | `10` | Burst limit for the Kubernetes API client |

## Cluster Settings

| Value | Default | Description |
|-------|---------|-------------|
| `controller.clusterDomain` | `cluster.local` | Kubernetes cluster domain used to construct service FQDNs. Change only if your cluster uses a non-default domain (e.g. `my-company.local`). |
| `controller.leaderElectionNamespace` | `""` (auto-detected) | Namespace for the leader election resource. |

## Deployment Example

Set values directly with `--set`:

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=v0.3.10 \
  --set controller.sandboxConcurrentWorkers=10 \
  --set controller.kubeApiQps=50 \
  --set controller.kubeApiBurst=100
```

Or use a values file:

```yaml
# custom-values.yaml
controller:
  sandboxConcurrentWorkers: 10
  sandboxClaimConcurrentWorkers: 10
  sandboxWarmPoolConcurrentWorkers: 10
  kubeApiQps: 50
  kubeApiBurst: 100
```

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=v0.3.10 \
  -f custom-values.yaml
```

To apply changes to a running release:

```bash
helm upgrade agent-sandbox ./helm/ --namespace agent-sandbox-system --reuse-values \
  --set controller.sandboxConcurrentWorkers=10 \
  --set controller.kubeApiQps=50 \
  --set controller.kubeApiBurst=100
```
