# Example deployment manifests

Drop-in starting point for running the Go sandbox-router in Kubernetes. These manifests prioritize sensible defaults over completeness — read each one and tune for your environment.

## Files

| File | What it does |
|---|---|
| `kustomization.yaml` | Core manifest set. Excludes the optional TokenReview binding and example ingress resources. |
| `serviceaccount.yaml` | Identity for the router pods. Implicit token automount is disabled; `deployment.yaml` projects a one-hour Kubernetes API token instead, which the API server cannot extend the way it extends the implicit mount. |
| `rbac.yaml` | ClusterRole + ClusterRoleBinding for `pods` get/list/watch. Required when `--cache-enabled=true`. The grant is cluster-wide on purpose — see the long-form comment at the top of the file for why narrowing to non-system namespaces isn't expressible in RBAC and how the runtime label selector keeps system Pods out of the cache anyway. Skip this file entirely when running DNS-only. |
| `rbac-tokenreview.yaml` | Extra ClusterRoleBinding to the stock `system:auth-delegator` ClusterRole. Apply *in addition to* `rbac.yaml` only when `--authz-mode=tokenreview`. Default-mode deployments don't carry these create rights on `tokenreviews.authentication.k8s.io` / `subjectaccessreviews.authorization.k8s.io` they wouldn't use. |
| `deployment.yaml` | 2 replicas, topology spread, distroless image, restricted SecurityContext, liveness/readiness probes. Enables `--cache-enabled=true` by default. |
| `service.yaml` | Cluster-IP service named `sandbox-router-svc` (preserves the Python router's name — existing Gateway/HTTPRoute resources work unchanged). |
| `pdb.yaml` | Prevents voluntary disruptions from taking the whole fleet offline. |
| `networkpolicy.yaml` | Locks down ingress to proxy/metrics/probe ports; egress to DNS, sandbox port, OTel collector. **Tighten the selectors for your tenancy model.** |
| `examples/gateway-gke.yaml` | Optional GKE Gateway, HTTPRoute, and HealthCheckPolicy for external ingress in front of `sandbox-router-svc`. |

## Apply

```sh
# Core router components. The kustomization excludes TokenReview RBAC.
kubectl apply -k sandbox-router/deploy/

# Optional: GKE Gateway API ingress.
# Note: GKE Standard clusters require Gateway API to be explicitly enabled
# using --gateway-api=standard. GKE Autopilot enables it by default.
kubectl apply -f sandbox-router/deploy/examples/gateway-gke.yaml
```

To run `--authz-mode=tokenreview`, also apply `rbac-tokenreview.yaml`; the kustomization leaves it out so default installs carry no TokenReview grant.

**Upgrading from `kubectl apply -f`.** Earlier revisions of this README applied the whole directory with `kubectl apply -f sandbox-router/deploy/`. That command now fails on `kustomization.yaml`, and it also applied `rbac-tokenreview.yaml` whether or not TokenReview was in use. Switch to `kubectl apply -k`. Apply does not delete resources it no longer lists, so a cluster that does not run `--authz-mode=tokenreview` can drop the leftover grant with `kubectl delete clusterrolebinding sandbox-router-auth-delegator`.

The base Deployment keeps `--authz-mode=allow-all` for compatibility. To run scoped-token v2, apply the [`../examples/scoped-token-v2/`](../examples/scoped-token-v2/README.md) overlay with `kubectl apply -k` in place of this directory. It uses this directory as its base and adds a plain strategic merge patch that mounts a versioned Ed25519 public-key set, so the first router Pods already require scoped tokens. Its README carries the reader-first key-rotation runbook. Downstream Helm and GitOps systems can model the same Pod contract without inheriting an upstream release policy.

## Things to change before production

1. **Image tag.** `deployment.yaml` uses `:latest`. Pin a real version once you publish one.
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
8. **Namespace access.** Anyone who can create Pods in `agent-sandbox-system` can run them as `sandbox-router` and project its token, whatever the automount setting, and so inherit its cluster-wide Pod read grant (and the TokenReview grant, if bound). Restrict Pod creation in that namespace to the operators who run the router.
