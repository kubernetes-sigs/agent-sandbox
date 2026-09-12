# Example deployment manifests

Drop-in starting point for running the Go sandbox-router in Kubernetes. These manifests prioritize sensible defaults over completeness — read each one and tune for your environment.

## Files

| File | What it does |
|---|---|
| `kustomization.yaml` | Kustomize resource list declaring the core components (`serviceaccount`, `rbac`, `deployment`, `service`, `pdb`). Excludes optional manifests (`rbac-tokenreview`, `networkpolicy`, `examples/`). |
| `serviceaccount.yaml` | Identity for the router pods. |
| `rbac.yaml` | ClusterRole + ClusterRoleBinding for `pods` get/list/watch. Required when `--cache-enabled=true`. The grant is cluster-wide on purpose — see the long-form comment at the top of the file for why narrowing to non-system namespaces isn't expressible in RBAC and how the runtime label selector keeps system Pods out of the cache anyway. Skip this file entirely when running DNS-only. |
| `rbac-tokenreview.yaml` | Extra ClusterRoleBinding to the stock `system:auth-delegator` ClusterRole. Apply *in addition to* `rbac.yaml` only when `--authz-mode=tokenreview`. Default-mode deployments don't carry these create rights on `tokenreviews.authentication.k8s.io` / `subjectaccessreviews.authorization.k8s.io` they wouldn't use. |
| `deployment.yaml` | 2 replicas, topology spread, distroless image, restricted SecurityContext, liveness/readiness probes. Enables `--cache-enabled=true` by default. |
| `service.yaml` | Cluster-IP service named `sandbox-router-svc` (preserves the Python router's name — existing Gateway/HTTPRoute resources work unchanged). |
| `pdb.yaml` | Prevents voluntary disruptions from taking the whole fleet offline. |
| `networkpolicy.yaml` | Locks down ingress to proxy/metrics/probe ports; egress to DNS, sandbox port, OTel collector. **Tighten the selectors for your tenancy model.** |
| `examples/gateway-gke.yaml` | Optional GKE Gateway, HTTPRoute, and HealthCheckPolicy for external ingress in front of `sandbox-router-svc`. |

## Apply

### Core components (recommended)

Use `kubectl apply -k` with kustomize to apply the core components (`serviceaccount.yaml`, `rbac.yaml`, `deployment.yaml`, `service.yaml`, `pdb.yaml`):

```sh
# Remote install from main:
kubectl apply -k "github.com/kubernetes-sigs/agent-sandbox//sandbox-router/deploy?ref=main"

# Or from a local clone:
kubectl apply -k sandbox-router/deploy/
```

### Optional components

These manifests provide additional security hardening for production environments:

- **Caller authentication (`rbac-tokenreview.yaml`):** Grants `system:auth-delegator` so the router can validate caller bearer tokens via TokenReview and SubjectAccessReview APIs. Apply when running with `--authz-mode=tokenreview` (omitted by default to follow least privilege when running unauthenticated):
  ```sh
  # Remote install:
  kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/sandbox-router/deploy/rbac-tokenreview.yaml

  # Or from a local clone:
  kubectl apply -f sandbox-router/deploy/rbac-tokenreview.yaml
  ```

- **Network isolation (`networkpolicy.yaml`):** Locks down ingress strictly to proxy/metrics/health probe ports, and egress to DNS, sandbox pods, and the apiserver. Review and tune selectors for your cluster CNI and Gateway namespace before applying:
  ```sh
  # Download to inspect and tune selectors:
  curl -sSLO https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/sandbox-router/deploy/networkpolicy.yaml
  kubectl apply -f networkpolicy.yaml

  # Or from a local clone:
  kubectl apply -f sandbox-router/deploy/networkpolicy.yaml
  ```

- **External ingress (`examples/gateway-gke.yaml`):** GKE Gateway API configuration to expose `sandbox-router-svc` externally:
  ```sh
  # Remote install:
  kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/sandbox-router/deploy/examples/gateway-gke.yaml

  # Or from a local clone:
  kubectl apply -f sandbox-router/deploy/examples/gateway-gke.yaml
  ```

## Things to change before production

1. **Image tag.** Pin to a specific release tag or digest as needed.
2. **Replica count.** 2 is the HA minimum, not a capacity recommendation. See "Scaling guidance" in the package README.
3. **Resource requests.** The defaults assume modest load. Right-size from load test numbers.
4. **NetworkPolicy selectors.** The example allows ingress from any namespace (`namespaceSelector: {}`). Tighten to your Gateway namespace.
5. **TLS.** The example is plain-HTTP. To enable TLS:
   - Add `--https-bind-address=:8443` and `--tls-cert-file` / `--tls-key-file` args.
   - Mount a Secret (cert-manager is the typical source) as a projected volume at `/tls`.
   - Uncomment the `proxy-tls` port in `service.yaml`.
   - Uncomment the `8443` ingress rule in `networkpolicy.yaml`.
6. **Observability.** Set `--enable-tracing` and `--enable-otel-metrics` and provide `OTEL_EXPORTER_OTLP_ENDPOINT` to push to your collector.
7. **HorizontalPodAutoscaler.** Not included by default. The router is CPU-bound at high RPS; a target CPU utilization HPA usually works. Use `sandbox_router_inflight_requests` as a custom metric if you want load-based scaling.
