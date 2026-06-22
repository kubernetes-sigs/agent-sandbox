# Agent Sandbox Helm Chart

This Helm chart installs the Agent Sandbox controller, which manages `Sandbox` resources on Kubernetes.
CRDs are bundled in the `crds/` directory and are installed automatically by Helm before any other resources.

## Installation

### Install from the Helm repository

Released chart versions are published to a Helm repository hosted on GitHub Pages.

```bash
helm repo add agent-sandbox https://kubernetes-sigs.github.io/agent-sandbox
helm repo update
helm install agent-sandbox agent-sandbox/agent-sandbox \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=<version>
```

To list the available chart versions:

```bash
helm search repo agent-sandbox --versions
```

### Install from a local checkout

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=<version>
```

### Install with extensions enabled

Extensions add support for `SandboxWarmPool`, `SandboxTemplate`, and `SandboxClaim` resources.

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=<version> \
  --set controller.extensions=true
```

### Install into an existing namespace

```bash
helm install agent-sandbox ./helm/ \
  --namespace my-namespace \
  --set image.tag=<version> \
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
| `image.tag` | Controller image tag — **required** | `""` |
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
| `podSecurityContext` | Pod `securityContext`; only rendered when set (e.g. Kyverno / Pod Security) | `null` |
| `containerSecurityContext` | Container `securityContext` for the controller; only rendered when set | `null` |
| `podAnnotations` | Annotations added to the controller pod template (e.g. service-mesh sidecar toggles, Prometheus scrape autodiscovery) | `{}` |
| `podLabels` | Extra labels added to the controller pod template alongside the chart's selector labels (selector labels take precedence on conflict) | `{}` |

## Publishing new chart versions

The [`helm-chart-release`](../.github/workflows/helm-chart-release.yml) workflow uses
[`chart-releaser-action`](https://github.com/helm/chart-releaser-action) to package the
chart and publish it to the Helm repository on the `gh-pages` branch.

To cut a new chart release, bump `version` in [`Chart.yaml`](./Chart.yaml) and merge to
`main`. The workflow only publishes a version that does not already have a corresponding
`helm-chart-agent-sandbox-<version>` release (the release name is set in
[`.github/cr.yaml`](../.github/cr.yaml)), so each release requires a version bump.

> **One-time setup**: GitHub Pages must be enabled for the repository with the source set
> to the `gh-pages` branch (Settings → Pages). The repository docs site is served
> separately via Netlify, so this branch is dedicated to the Helm repository index.

