# 50-router: external access to the fleet

Deploys the data plane that puts every employee's OpenClaw UI behind one external endpoint. See the main example README for the full walkthrough; this directory only covers the routing layer.

## What gets deployed

- **`10-router.yaml`** — the Go [sandbox-router](../../../sandbox-router/README.md) (2 replicas) with the Pod-IP cache enabled (namespace-scoped Role instead of the upstream ClusterRole) and path routing enabled under `/router`, plus its `sandbox-router-svc` Service and a NetworkPolicy.
- **`20-gateway.yaml`** — a GKE Gateway (`gke-l7-global-external-managed`) named `openclaw-fleet-gateway`, an HTTPRoute sending `/router` to the sandbox-router, a catch-all HTTPRoute sending everything else to `fleet-portal:8080`, and `HealthCheckPolicy` objects pointing both backends' health checks at `/healthz`.
- **`30-iap-optional.yaml`** — fully commented-out `GCPBackendPolicy` objects that attach IAP to both services; see the file for prerequisites (OAuth client, `roles/iap.httpsResourceAccessor`, HTTPS listener, Workforce Identity Federation for SAML/OIDC IdPs).

## The URL shape

```text
https://<gateway-ip>/router/openclaw-fleet/oc-<employee-id>/18789/
```

The `/router` prefix hands the request to the sandbox-router, which parses the remaining segments as `<namespace>/<id>/<port>` and resolves `oc-<employee-id>` via DNS — that name is the per-employee ExternalName Service the fleet-portal maintains, aliasing the adopted sandbox's headless service. No per-employee Gateway or HTTPRoute objects are ever created.

WebSockets work through this path unchanged: the router forwards `Connection: Upgrade` transparently, exempts upgraded connections from its proxy timeout, and strips `Origin` on upgrades so Origin-checking backends don't reject the handshake. Because the routing identity lives in the URL path, the relative WebSocket URLs OpenClaw's own frontend opens inherit it for free.

## Authorization

The demo runs the router with its default `allow-all` authorizer — anyone who can reach the Gateway can reach any sandbox. For production, per the router README:

- `--authz-mode=tokenreview` — authenticate Bearer tokens against the cluster's TokenReview API (needs the `system:auth-delegator` binding from `sandbox-router/deploy/rbac-tokenreview.yaml`).
- `--authz-mode=scoped-token` — per-sandbox capability tokens minted outside the router.
- `--authz-cookie-name` / `--authz-cookie-query-param` — bootstrap a path-scoped session cookie so browser traffic (which cannot set an `Authorization` header) can present either credential.
- Or terminate authentication before the cluster entirely with IAP (`30-iap-optional.yaml`).
