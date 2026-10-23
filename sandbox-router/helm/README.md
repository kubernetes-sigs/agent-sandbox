# Sandbox Router Helm Chart

This Helm chart installs the Go [sandbox-router](../README.md), the reverse proxy that routes SDK and browser traffic to sandboxes. It renders the same resources as the kustomize base in [`../deploy/`](../deploy/), with the optional pieces (TLS, TokenReview RBAC, namespace-scoped cache RBAC) behind values.

The chart does not create the namespace. It defaults to `agent-sandbox-system`, which the controller chart creates; use `--create-namespace` when installing the router on its own.

## Installation

```bash
helm install sandbox-router ./sandbox-router/helm/ \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=<version>
```

`image.tag` is required. The kustomize base pins `:latest`, which the chart deliberately does not.

### TokenReview authorization

```bash
helm install sandbox-router ./sandbox-router/helm/ \
  --namespace agent-sandbox-system \
  --set image.tag=<version> \
  --set authz.mode=tokenreview
```

This also binds the stock `system:auth-delegator` ClusterRole to the router ServiceAccount. The binding is rendered only in this mode, so default installs carry no TokenReview grant.

### TLS

Create a Secret with `tls.crt` and `tls.key` (cert-manager is the typical source), then:

```bash
helm install sandbox-router ./sandbox-router/helm/ \
  --namespace agent-sandbox-system \
  --set image.tag=<version> \
  --set tls.enabled=true \
  --set tls.secretName=sandbox-router-tls
```

This adds the HTTPS listener, mounts the Secret at `/tls`, and opens port 8443 on the Service and the NetworkPolicy.

### Scoped-token authorization

The chart does not model the verification key set. Mount it with `extraVolumes` / `extraVolumeMounts` and point the router at it with `router.extraArgs`; see [`ci/scoped-token-values.yaml`](ci/scoped-token-values.yaml) for the shape and [`../examples/scoped-token-v2/`](../examples/scoped-token-v2/README.md) for the key-rotation runbook. Use a new ConfigMap name for every rotation so the Pod template changes and the Deployment rolls.

## Upgrading

```bash
helm upgrade sandbox-router ./sandbox-router/helm/ \
  --namespace agent-sandbox-system \
  --reuse-values \
  --set image.tag=<new-version>
```

### Moving from the kustomize base

The chart uses the same resource names as `kubectl apply -k sandbox-router/deploy/`, so Helm refuses to adopt resources it did not create. Delete the kustomize-applied router first, or label and annotate the existing resources for Helm adoption:

```bash
kubectl delete -k sandbox-router/deploy/
```

## Uninstallation

```bash
helm uninstall sandbox-router --namespace agent-sandbox-system
```

## Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.tag` | Router image tag, **required** | `""` |
| `image.repository` | Router image repository | `registry.k8s.io/agent-sandbox/sandbox-router-go` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Image pull secrets for the Deployment | `[]` |
| `replicaCount` | Number of router replicas (2 is the HA minimum) | `2` |
| `namespace.name` | Namespace to deploy into, falls back to the release namespace if empty | `agent-sandbox-system` |
| `router.clusterDomain` | Kubernetes cluster domain used to build sandbox FQDNs | `cluster.local` |
| `router.proxyTimeout` | Per-request upstream timeout | `180s` |
| `router.upstreamMaxRetries` | Dial retries, `0` disables | `3` |
| `router.cacheEnabled` | Enable the Pod-IP cache; requires the Pod read RBAC | `true` |
| `router.cacheNamespace` | Restrict the Pod informer and the RBAC to one namespace, empty means cluster-wide | `""` |
| `router.enableTracing` | OTel traces via OTLP | unset (router default, auto) |
| `router.enableOtelMetrics` | Push metrics via OTLP | unset (router default, auto) |
| `router.accessLog` | One structured log line per request | unset (router default `true`) |
| `router.maxRequestBodyBytes` | Cap on inbound body size | unset (router default `0`, unlimited) |
| `router.pathRoutingPrefix` | Path prefix for callers that cannot set `X-Sandbox-*` headers | unset (disabled) |
| `router.extraArgs` | Additional flags not listed above | `[]` |
| `authz.mode` | `allow-all`, `tokenreview` or `scoped-token` | `allow-all` |
| `tls.enabled` | Enable the HTTPS listener and mount the TLS Secret | `false` |
| `tls.secretName` | Secret holding `tls.crt` and `tls.key` | `sandbox-router-tls` |
| `tls.metricsSecureServing` | Serve `/metrics` over TLS with the same certificate | `false` |
| `extraEnv` | Extra environment variables, e.g. `OTEL_EXPORTER_OTLP_ENDPOINT` | `[]` |
| `extraVolumes` / `extraVolumeMounts` | Extra volumes and mounts, e.g. the scoped-token key set | `[]` |
| `podAnnotations` | Pod annotations; the default lets Prometheus scrape port 9090 | `prometheus.io/*` scrape annotations |
| `podLabels` | Extra Pod labels (selector labels take precedence) | `{}` |
| `resources` | CPU and memory requests and limits | `250m` / `256Mi` requests, `2` / `1Gi` limits |
| `podSecurityContext` | Pod `securityContext`, rendered only when set | `null` |
| `containerSecurityContext` | Container `securityContext` | restricted (non-root, read-only rootfs, no capabilities) |
| `topologySpread.enabled` | Spread replicas across zones and nodes | `true` |
| `nodeSelector` / `tolerations` / `affinity` | Scheduling constraints | `{}` / `[]` / `{}` |
| `terminationGracePeriodSeconds` | Grace period, keep above the router's `--shutdown-timeout` (30s) | `45` |
| `service.name` | Service name, kept so existing Gateway/HTTPRoute resources work | `sandbox-router-svc` |
| `service.type` | Service type | `ClusterIP` |
| `serviceAccount.annotations` | ServiceAccount annotations, e.g. for workload identity | `{}` |
| `rbac.create` | Render the Pod read RBAC (and the auth-delegator binding in tokenreview mode) | `true` |
| `pdb.enabled` | Create a PodDisruptionBudget | `true` |
| `pdb.minAvailable` | Minimum available replicas | `1` |
| `networkPolicy.enabled` | Create the NetworkPolicy | `true` |
| `networkPolicy.proxyIngressFrom` | Peers allowed to reach the proxy ports, tighten to your Gateway namespace | all namespaces |
| `networkPolicy.metricsIngressFrom` | Peers allowed to scrape the metrics port | `monitoring` namespace |

Before production, read "Things to change before production" in the [deploy README](../deploy/README.md): replica count, resource requests, NetworkPolicy selectors, TLS and an HPA apply to the chart as well.

## Development

The chart mirrors the kustomize base in [`../deploy/`](../deploy/). When one changes, update the other, and bump `version` in `Chart.yaml` when the chart changes.

`make lint-helm` lints the chart with [chart-testing](https://github.com/helm/chart-testing), running `helm lint` once per `ci/*-values.yaml` file. Add a values file there when adding a template that is only rendered under non-default values.
