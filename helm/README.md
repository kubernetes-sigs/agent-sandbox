# Agent Sandbox Helm Chart

This Helm chart installs the Agent Sandbox controller, which manages `Sandbox` resources on Kubernetes.
CRDs are bundled in the `crds/` directory and are installed automatically by Helm before any other resources.

## Installation

### Basic install

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=v0.3.10
```

### Install with extensions enabled

Extensions add support for `SandboxWarmPool`, `SandboxTemplate`, and `SandboxClaim` resources.

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=v0.3.10 \
  --set controller.extensions=true
```

### Install into an existing namespace

```bash
helm install agent-sandbox ./helm/ \
  --namespace my-namespace \
  --set image.tag=v0.3.10 \
  --set namespace.create=false \
  --set namespace.name=my-namespace
```

## Upgrading

```bash
helm upgrade agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --reuse-values \
  --set image.tag=<new-version>
```

> **Note**: Helm does not upgrade CRDs placed in `crds/` automatically. To update CRDs manually after a chart version bump, apply them directly:
>
> ```bash
> kubectl apply -f helm/crds/
> ```

## Uninstallation

```bash
helm uninstall agent-sandbox --namespace agent-sandbox-system
```

> **Note**: Helm does not delete CRDs on uninstall. To remove all CRDs and their associated custom resources:
>
> ```bash
> kubectl delete -f helm/crds/
> ```
>
> Warning: This will delete **all** `Sandbox`, `SandboxWarmPool`, `SandboxTemplate`, and `SandboxClaim` objects across all namespaces.

## Configuration

The following table lists the configurable parameters and their defaults.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.tag` | Controller image tag — **required** | `"v0.3.10"` |
| `image.repository` | Controller image repository | `registry.k8s.io/agent-sandbox/agent-sandbox-controller` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `replicaCount` | Number of controller replicas | `1` |
| `namespace.create` | Create the namespace as part of the release | `true` |
| `namespace.name` | Namespace to deploy into | `agent-sandbox-system` |
| `controller.leaderElect` | Enable leader election | `true` |
| `controller.leaderElectionNamespace` | Namespace for the leader election resource (auto-detected if empty) | `""` |
| `controller.clusterDomain` | Kubernetes cluster domain for service FQDN generation | `"cluster.local"` |
| `controller.kubeApiQps` | QPS limit for the Kubernetes API client (`-1` = unlimited) | `-1.0` |
| `controller.kubeApiBurst` | Burst limit for the Kubernetes API client | `10` |
| `controller.sandboxConcurrentWorkers` | Max concurrent reconciles for the Sandbox controller | `1` |
| `controller.sandboxClaimConcurrentWorkers` | Max concurrent reconciles for the SandboxClaim controller (extensions only) | `1` |
| `controller.sandboxWarmPoolConcurrentWorkers` | Max concurrent reconciles for the SandboxWarmPool controller (extensions only) | `1` |
| `controller.sandboxTemplateConcurrentWorkers` | Max concurrent reconciles for the SandboxTemplate controller (extensions only) | `1` |
| `controller.enableTracing` | Enable OpenTelemetry tracing via OTLP | `false` |
| `controller.enablePprof` | Enable CPU profiling endpoint on the metrics server | `false` |
| `controller.enablePprofDebug` | Enable all pprof endpoints (implies enablePprof) | `false` |
| `controller.pprofBlockProfileRate` | Block profile sampling rate when pprof debug is enabled | `1000000` |
| `controller.pprofMutexProfileFraction` | Mutex contention sampling rate when pprof debug is enabled | `10` |
| `controller.extraArgs` | Additional flags not listed above (e.g. zap logging flags) | `[]` |
| `controller.extensions` | Enable extensions controller (WarmPool, Template, Claim) | `false` |
| `resources` | CPU/memory resource requests and limits | `{}` |
| `nodeSelector` | Node selector for the controller pod | `{}` |
| `tolerations` | Tolerations for the controller pod | `[]` |
| `affinity` | Affinity rules for the controller pod | `{}` |
