# agent-sandbox Helm Chart

A Helm chart for deploying the [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller.

## Introduction

This chart bootstraps an `agent-sandbox` deployment on a [Kubernetes](http://kubernetes.io) cluster using the [Helm](https://helm.sh) package manager.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+

## Installing the Chart

To install the chart with the release name `my-release`:

```bash
helm install my-release ./charts/agent-sandbox
```

## Uninstalling the Chart

To uninstall/delete the `my-release` deployment:

```bash
helm delete my-release
```

## Configuration

The following table lists the configurable parameters of the agent-sandbox chart and their default values.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Controller image repository | `ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller` |
| `image.tag` | Controller image tag | `latest` (overridden by chart appVersion) |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `serviceAccount.create` | Specifies whether a ServiceAccount should be created | `true` |
| `serviceAccount.name` | The name of the ServiceAccount to use | `agent-sandbox-controller` |
| `extensions.enabled` | Enable extension controllers (SandboxClaim, SandboxWarmPool) | `false` |
| `resources` | CPU/Memory resource requests/limits | `{}` |

Specify each parameter using the `--set key=value[,key=value]` argument to `helm install`. For example,

```bash
helm install my-release ./charts/agent-sandbox --set extensions.enabled=true
```

Alternatively, a YAML file that specifies the values for the above parameters can be provided while installing the chart. For example,

```bash
helm install my-release ./charts/agent-sandbox -f values.yaml
```
