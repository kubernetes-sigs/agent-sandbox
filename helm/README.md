# Agent Sandbox Helm Chart

This Helm chart installs the Agent Sandbox controller, which manages `Sandbox` resources on Kubernetes.
CRDs are bundled in the `crds/` directory and are installed automatically by Helm before any other resources.

## Installation

### Basic install (core controller only)

```bash
helm install agent-sandbox ./helm/ \
  --namespace agent-sandbox-system \
  --create-namespace
```

### Install with a specific image tag

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
  --set extensions.enabled=true
```

### Install into an existing namespace

```bash
helm install agent-sandbox ./helm/ \
  --namespace my-namespace \
  --set namespace.create=false \
  --set namespace.name=my-namespace
```

## Upgrading

```bash
helm upgrade agent-sandbox ./helm/ \
  --namespace agent-sandbox-system
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
| `namespace.create` | Create the namespace as part of the release | `true` |
| `namespace.name` | Namespace to deploy into | `agent-sandbox-system` |
| `image.repository` | Controller image repository | `registry.k8s.io/agent-sandbox/agent-sandbox-controller` |
| `image.tag` | Image tag | `"v0.3.10"` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `replicaCount` | Number of controller replicas | `1` |
| `controller.leaderElect` | Enable leader election | `true` |
| `controller.extraArgs` | Additional arguments passed to the controller binary | `[]` |
| `extensions.enabled` | Enable extensions controller (WarmPool, Template, Claim) | `false` |
| `resources` | CPU/memory resource requests and limits | `{}` |
| `nodeSelector` | Node selector for the controller pod | `{}` |
| `tolerations` | Tolerations for the controller pod | `[]` |
| `affinity` | Affinity rules for the controller pod | `{}` |
