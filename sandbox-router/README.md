# Sandbox Router (Go)

A Go reimplementation of the Python [`sandbox-router`](../clients/python/agentic-sandbox-client/sandbox-router/README.md) — a small reverse proxy that fans HTTP traffic out to thousands of ephemeral agent sandbox pods in a Kubernetes cluster.

It preserves the original `X-Sandbox-*` header contract (so existing clients and `Gateway` / `HTTPRoute` resources keep working) and adds the controls needed for enterprise deployments: TLS, mTLS, Prometheus metrics, OpenTelemetry tracing, hot-reloading certs, dial retries, structured logs, and graceful shutdown.

## Where it sits

Agent Sandbox splits creation from routing. The router only handles the **data plane**:

```text
                    creates                  watches
  SDK client  ─────────────────►  K8s API  ─────────►  Controller  ── creates Pod + Service
  (clients/go/sandbox or
   the Python SDK)

  ┌─────────────┐ X-Sandbox-ID  ┌──────────┐  HTTP   ┌──────────┐
  │ HTTP client │  ───────────► │  Router  │ ──────► │ Sandbox  │
  │ (curl, SDK) │               │ (stateless)│       │   Pod    │
  └─────────────┘               └──────────┘         └──────────┘
```

The SDKs (Go and Python) hard-code `sandbox-router-svc` as their routing target and set the `X-Sandbox-*` headers automatically. A user calling `sb.Run(...)` never sees the router. A raw HTTP client (`curl`, your own code) needs to know the contract below.

The router **never** creates or looks up Sandbox resources. If the target sandbox doesn't exist, the request fails with 502 (after a short retry window — see [Behavior on missing sandboxes](#behavior-on-missing-sandboxes)).

## Request contract

| Header | Required | Default | Notes |
|---|---|---|---|
| `X-Sandbox-ID` | yes | — | Sandbox pod name. With `--cache-enabled=true`, combined with the namespace to look up the live PodIP in the cache's name index; otherwise (or on miss) used as the host component of the DNS form. |
| `X-Sandbox-UID` | no | — | Sandbox CR UID. When `--cache-enabled=true` and the Pod-IP cache has an entry for this UID, the router dials the cached live PodIP and bypasses DNS — the KEP-NNNN fast path. Cache miss falls through to the name index, then the DNS form. |
| `X-Sandbox-Namespace` | no | `default` | Must be ASCII letters / digits / hyphens, with at least one alphanumeric. |
| `X-Sandbox-Port` | no | `8888` | Numeric. |
| `X-Sandbox-Pod-IP` | no | — | When set, bypasses both cache and DNS and dials this IP directly. |

Resolution priority (first match wins):

1. `X-Sandbox-Pod-IP` — explicit caller override, used by SDKs that already know the Pod IP.
2. Cache lookup by `X-Sandbox-UID` — KEP-NNNN's secure fast path. Only attempted when `--cache-enabled=true` and the UID header is present.
3. Cache lookup by `X-Sandbox-Namespace`/`X-Sandbox-ID` — the sandbox name is the Pod name, so the cache's name index resolves the same live Pod IP without a UID. Like step 2, only attempted when `--cache-enabled=true`; deployments running with the cache disabled fall straight through to step 4. This is what keeps warm-pool sandboxes (which have no per-sandbox Service, so step 4 is guaranteed NXDOMAIN) routable for SDK traffic that carries no UID or Pod IP. Warm-pool Pods that no SandboxClaim has adopted yet are excluded from the name index — they remain reachable only by UID, preserving the UID-as-capability property so callers cannot reach an unclaimed pool Pod by guessing pool-generated names.
4. DNS form — compatibility fallback used without an informer cache or after cache misses; matches the Python router's behavior.

The router constructs the upstream URL as:
- DNS form: `http://<ID>.<Namespace>.svc.<cluster-domain>:<port>/<path>?<query>`
- Pod-IP form (cache hit or override): `http://<Pod-IP>:<port>/<path>?<query>`

It strips the inbound `Host` and `Authorization` headers before forwarding. `Host` is stripped so `net/http` picks the upstream URL's host; `Authorization` is stripped because the router consumes it (e.g. `--authz-mode=tokenreview` validates a Bearer token via the K8s TokenReview API) and forwarding the credential to the sandbox would let any sandbox impersonate the caller against the K8s API. All other headers pass through.

### Input validation

The router validates the routing headers before constructing the upstream URL, matching the Python router's checks:

| Input | Check |
|---|---|
| `X-Sandbox-ID` | required; must be a valid DNS-1123 label (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 chars). Rejects DNS injection inputs like `foo.evil.com` and traversal-style inputs like `foo/bar`. |
| `X-Sandbox-Namespace` | optional (defaults to `default`); same DNS-1123 label check. |
| `X-Sandbox-Port` | optional (defaults to `8888`); must parse as integer in `[1, 65535]`. |
| `X-Sandbox-Pod-IP` | optional; must be a valid IP literal AND not loopback / link-local / multicast / unspecified. The class check is the SSRF defense — without it, a caller could set `X-Sandbox-Pod-IP: 169.254.169.254` and have the router proxy to cloud metadata. With `AllowAll` as the default authorizer (Python compatibility), this validation is the only thing preventing the gadget. See `--allow-loopback-pod-ip` for the sidecar case. |
| `X-Sandbox-UID` | optional; used as cache lookup key only, no further validation. |

### Endpoints

- `GET /healthz` → `200 OK` with `{"status":"ok"}` (matches the Python contract; used by Gateway health checks, e.g. GKE HealthCheckPolicy)
- Anything else → reverse-proxied to the resolved sandbox

### Error responses

Errors are JSON with a single `detail` field — same shape as the Python router:

| Cause | Status | Body |
|---|---|---|
| Missing `X-Sandbox-ID` | 400 | `{"detail":"X-Sandbox-ID header is required."}` |
| Invalid `X-Sandbox-ID` (not a DNS label) | 400 | `{"detail":"Invalid sandbox ID format."}` |
| Invalid `X-Sandbox-Namespace` | 400 | `{"detail":"Invalid namespace format."}` |
| `X-Sandbox-Port` not numeric or out of `[1, 65535]` | 400 | `{"detail":"Invalid port format."}` |
| `X-Sandbox-Pod-IP` malformed or in a rejected class | 400 | `{"detail":"Invalid target IP address."}` |
| Upstream dial fails (after retries) | 502 | `{"detail":"Could not connect to the backend sandbox: <id>"}` |

## Behavior on missing sandboxes

When the target sandbox can't be dialed (DNS doesn't resolve, or the pod isn't listening), the router retries with exponential backoff before giving up with 502. This smooths the case where a sandbox was just created and DNS / the listener hasn't caught up yet. Only **dial-class** failures are retried — failures after the request body may have been sent (response timeouts, mid-stream EOF) bubble up immediately because replaying a partially-sent body could duplicate side effects.

Defaults: 3 retries (4 attempts total), 200 ms → 400 ms → 800 ms backoff. Tunable via `--upstream-max-retries`, `--upstream-retry-initial-delay`, `--upstream-retry-max-delay`. Set retries to `0` to disable.

The retry budget never exceeds `--proxy-timeout`; the per-request context cuts it short.

## WebSockets and other protocol upgrades

`Connection: Upgrade` / `Upgrade: websocket` requests are forwarded transparently via `httputil.ReverseProxy`'s built-in upgrade handling. This is what makes things like `code-server` (VS Code in the browser, holds a single long-lived WebSocket per editing session) work through the router unchanged.

Two carve-outs that matter in practice:

- **`--proxy-timeout` does NOT apply to upgraded connections.** A WebSocket is long-lived by design, so we cancel only the per-request context for non-upgraded HTTP; once the `101 Switching Protocols` response has gone back to the client, the connection's TCP keepalive is the liveness signal. Without this carve-out, the 180s default would tear down a healthy WebSocket at the 3-minute mark and the client would see WebSocket close `1006`.
- **`Origin` is stripped from upgrade requests.** Many WebSocket backends (vscode-server is the classic case; Jupyter behaves the same) validate `Origin` against `Host` for CSRF protection. The router rewrites `Host` to the upstream sandbox's address, so a client-supplied `Origin` pointing at the router's external hostname would mismatch and the backend would reject the upgrade — same `1006` symptom. Dropping `Origin` tells the backend "no Origin assertion available," which CSRF-aware backends typically allow for non-browser callers. Normal (non-upgrade) HTTP traffic preserves `Origin` so CORS preflights are unaffected.

The router also sets `X-Forwarded-Host` / `X-Forwarded-Proto` / `X-Forwarded-For` on every outbound request (via `httputil.ReverseProxy`'s `SetXForwarded` helper), so upstream sandboxes can reconstruct the client-visible URL for self-links and redirects — important for browser-facing backends like Jupyter and vscode-server.

Metrics for upgraded requests record `code="101"` once the handshake completes; the duration histogram records the full lifetime of the upgraded connection.

### Browser-facing traffic: path-based routing

Everything above assumes the caller can set `X-Sandbox-*` headers. A browser tab, an `<iframe>`, and — critically — a WebSocket handshake cannot: there is no web API for attaching custom headers to a top-level navigation, a subresource document load, or `new WebSocket(url, protocols)` (`protocols` only negotiates a subprotocol, it is not a headers map). This is a platform limitation, not something a Service Worker can work around either — WebSocket handshakes are explicitly not exposed to the `fetch` event, which is exactly why tools that need to intercept WebSocket traffic client-side (e.g. Mock Service Worker) have to patch the global `WebSocket` constructor instead of using a Service Worker at all. See [w3c/ServiceWorker#947](https://github.com/w3c/ServiceWorker/issues/947), still open.

This bites concretely for any browser-facing dev tool proxied through this router — a web IDE whose terminal is a WebSocket (code-server is the common case), or a dev server whose hot-reload client opens one (Vite/webpack HMR). Both work fine once a request reaches the router with the right headers; the problem is entirely upstream of that, in getting the browser's own request there in the first place.

Set `--path-routing-prefix` to give such traffic a second way in: a request whose path starts with the configured prefix is routed by `<prefix>/<namespace>/<id>/<port>/<rest...>` instead of headers — plain URL segments, which a browser can put in any navigation, `<iframe src>`, or `new WebSocket(...)` call with zero client-side code. Concretely, once a page is loaded at a URL under this prefix, any *relative* WebSocket URL that page's own JavaScript opens (which is how code-server and most dev-server HMR clients behave) automatically carries the same prefix along — the routing identity travels for free.

This is strictly additive: the default is `""` (disabled), and a request whose path does not match the configured prefix falls straight through to `X-Sandbox-*` header parsing, unchanged. `X-Sandbox-Pod-IP` and `X-Sandbox-UID` have no path equivalent — both are dial-target overrides meant for SDKs that already hold cluster-internal knowledge, never for a browser tab, so path-routed requests always resolve through the DNS form or the namespace/name cache index, same as a header-routed request carrying only `X-Sandbox-ID`.

**Enabling this flag claims the entire URL namespace under the prefix.** The fallback above only applies to a path that does not match the prefix as a path segment — `/routerish/...` shares the literal string `/router` but does not match, because there is no `/` boundary right after it, so it still falls through to headers like today. Once a request's path *does* match, it is committed to path-based routing and stays there — a malformed one (missing the port segment, an invalid namespace or ID, and so on) gets a `400` from `ParsePathRoute` directly and is **not** retried against `X-Sandbox-*` headers, even if the caller happened to also send them. Pick a prefix no real application route inside a sandbox needs to answer under, for exactly this reason.

Everything after `<port>` is forwarded byte-for-byte, escaping included — an encoded separator inside a single upstream path segment (e.g. a filename containing `/`, sent as `%2F`) survives the hop unchanged rather than being silently decoded into an extra segment. `sandbox_router_requests_total` labels, the access log's `sandbox_id`/`sandbox_namespace` fields, and the `sandbox.id`/`sandbox.namespace` trace attributes all reflect the resolved target regardless of whether it came from a header or from the path.

```sh
curl -i --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  'http://sandbox-router-svc:8080/router/default/my-sandbox/8080/'
# -> 101 Switching Protocols, no X-Sandbox-* header sent anywhere in this request
```

## Flags

Run `sandbox-router --help` for the full list. The most relevant:

| Flag | Default | Notes |
|---|---|---|
| `--http-bind-address` | `:8080` | Plain-HTTP proxy listener. Empty disables. |
| `--https-bind-address` | `""` | TLS proxy listener. Empty disables. Requires `--tls-cert-file` and `--tls-key-file`. |
| `--metrics-bind-address` | `:9090` | Prometheus `/metrics`. |
| `--health-probe-bind-address` | `:8081` | `/healthz` and `/readyz`. |
| `--tls-cert-file` / `--tls-key-file` | — | PEM-encoded server cert and key. Hot-reloaded on file change (via fsnotify on the parent directory, so atomic Secret rotation just works). |
| `--tls-client-ca-file` | — | CA bundle for verifying client certs when mTLS is on. |
| `--mtls-mode` | `off` | `off` / `optional` / `required`. |
| `--cluster-domain` | `cluster.local` | Honors `CLUSTER_DOMAIN` env var (Python parity). |
| `--proxy-timeout` | `180s` | Per-request upstream timeout. Honors `PROXY_TIMEOUT_SECONDS` (numeric seconds). |
| `--upstream-max-retries` | `3` | Dial retries. `0` disables. |
| `--max-request-body-bytes` | `0` (unlimited) | Optional cap on inbound body size. |
| `--allow-loopback-pod-ip` | `false` | Permit loopback addresses in `X-Sandbox-Pod-IP`. Default-off rejects the router's own loopback as an SSRF target. Enable only when the sandbox runs as a sidecar in the router's Pod, or for integration tests against a localhost backend. Link-local / multicast / unspecified stay rejected regardless. |
| `--path-routing-prefix` | `""` (disabled) | Optional path prefix (`<prefix>/<namespace>/<id>/<port>/...`) for callers that cannot set `X-Sandbox-*` headers — see [Browser-facing traffic](#browser-facing-traffic-path-based-routing). Falls through to header-based routing for any path that doesn't match. |
| `--cache-enabled` | `false` | Enable the Pod-IP cache (KEP-NNNN fast path). Requires the RBAC in `deploy/rbac.yaml`. |
| `--cache-namespace` | `""` (cluster-wide) | Restrict the Pod informer to a single namespace. |
| `--kubeconfig` | `""` (in-cluster) | Kubeconfig for the cache's informer client. Honors `KUBECONFIG`. |
| `--enable-tracing` | auto | OTel traces via OTLP gRPC. Auto-enabled when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set; pass `--enable-tracing=false` to override. |
| `--enable-otel-metrics` | auto | Additionally push metrics via OTLP gRPC. Auto-enabled when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` is set; Prometheus `/metrics` stays active either way. |
| `--access-log` | `true` | One structured log line per request on the proxy port (skips `/healthz`, `/readyz`, `/metrics`). |
| `--config` | `""` | Path to a YAML config file. Honors `SANDBOX_ROUTER_CONFIG`. |
| `--shutdown-timeout` | `30s` | Drain budget on SIGTERM. |

## Pod-IP cache (KEP-NNNN fast path)

When `--cache-enabled=true`, the router runs an in-process Kubernetes informer that watches sandbox-owned Pods cluster-wide (or scoped to `--cache-namespace`) and maintains a UID → live PodIP map plus a namespace/name secondary index over the same entries. The informer filters server-side on the `agents.x-k8s.io/sandbox-name-hash` label that the controller stamps on every sandbox Pod, so memory and API traffic scale with the number of sandboxes — not the size of the cluster.

For every inbound request, the proxy resolves the upstream in this order: explicit `X-Sandbox-Pod-IP` header → cache lookup by `X-Sandbox-UID` → cache lookup by `X-Sandbox-Namespace`/`X-Sandbox-ID` (the name index) → DNS form. Cache hits skip the DNS resolution hop entirely, which is the property the KEP requires for high-throughput tenants. The name-index step matters most for warm-pool sandboxes: they have no per-sandbox Service, so for traffic that carries only the ID header the DNS form can never resolve and the name index is the only working path. Cache misses fall through to DNS — the router never refuses to route a request just because the cache is cold or out of sync.

**Active invalidation.** When the proxy dials an IP that came from the cache and the dial fails because the host is unreachable (timeout, no route — the Pod was rescheduled and the cache hasn't caught up), the cache entry is evicted immediately — by UID when the UID path resolved it, by namespace/name when the name index did — so the next request for the same sandbox falls through to DNS instead of retrying the same stale IP. A connection *refusal* does not evict: it proves a live host (typically a caller-selected port the Pod isn't listening on), and evicting on it would strand warm-pool traffic on the DNS path until the next resync. Name-keyed evictions are additionally conditional on the entry still holding the IP that failed, so a recreated Pod cached mid-dial is never evicted by a late failure against its predecessor's IP. This is the resilience guarantee called out in the KEP. The `sandbox_router_cache_invalidations_total` counter tracks how often this fires.

**Cache content.** Only Pods that pass `PodReady=True` and have a non-empty `Status.PodIP` are stored. Pods that flip out of Ready are removed automatically by the informer event handler so traffic doesn't get steered at a degraded Pod. Unclaimed warm-pool Pods are stored but not name-indexed (see resolution priority above); adoption by a SandboxClaim removes the `agents.x-k8s.io/warm-pool-sandbox` label and the resulting Pod update makes the entry name-routable.

**RBAC.** Cluster-wide `get`, `list`, `watch` on `pods`. The example `deploy/rbac.yaml` is a `ClusterRole` + `ClusterRoleBinding`; narrow to a `Role` + `RoleBinding` when `--cache-namespace` is set. Note that K8s RBAC has no negative-namespace primitive, so the grant cannot say "all namespaces except kube-system" — the runtime label selector (`agents.x-k8s.io/sandbox-name-hash`) is what keeps system Pods out of the actual watch and the cache. The file's header comment spells this out for auditors.

**Readiness gating.** The router's `/readyz` does not flip to ready until the initial Pod LIST has completed. A misconfigured RBAC therefore fails fast at startup rather than silently degrading the router to DNS-only service.

**When to leave it off.** The DNS-only mode (default) is appropriate for small deployments, for clusters where you don't want to grant Pod read permissions to the router, or for testing. Everything else continues to work — the cache is purely additive.

## Authorization

The router runs every request through an `authz.Authorizer` after header parsing and before resolving the upstream. The default is `authz.AllowAll`, which preserves the Python router's no-auth contract: anything that reaches the router with a valid `X-Sandbox-ID` is forwarded. Two other built-in authorizers — TokenReview and scoped-token, both described below — are selectable via `--authz-mode`.

The `Authorizer` interface is intentionally simple:

```go
type Authorizer interface {
    Authorize(ctx context.Context, r *http.Request, sandboxNamespace, sandboxName string) error
}
```

Returning `nil` allows the request; returning `authz.ErrUnauthenticated` produces a 401 JSON response, `authz.ErrForbidden` produces 403, anything else produces 500. Implementations pull whatever credential they need (Bearer token via `authz.BearerTokenFromRequest`, TLS client cert, custom header) directly off the request.

The `sandbox_router_authz_decisions_total{decision="allow|deny",sandbox_namespace="…"}` counter records every verdict so deployments can see whether `AllowAll` is actually allowing the traffic shape they expect.

### TokenReview authorizer

Set `--authz-mode=tokenreview` to enable the built-in authorizer that authenticates every request by submitting its `Authorization: Bearer <token>` header to the cluster's `authentication.k8s.io/v1.TokenReview` API. The decision (positive or negative) is cached in an LRU by SHA-256 hash of the token — raw tokens are never stored — for `--authz-tokenreview-ttl` (default `30s`). Apiserver-error responses are cached briefly (1/3 of the TTL, minimum 1s) so a flapping apiserver doesn't get pummeled but transient failures self-heal quickly.

Flags:

| Flag | Default | Notes |
|---|---|---|
| `--authz-mode` | `allow-all` | `allow-all`, `tokenreview`, or `scoped-token`. |
| `--authz-tokenreview-ttl` | `30s` | Cache TTL for both positive and negative decisions. |
| `--authz-tokenreview-cache-size` | `2048` | LRU bound. |
| `--authz-tokenreview-require-token` | `false` | When false, tokenless requests pass (transitional). When true, missing token → 401. |
| `--authz-tokenreview-audiences` | `""` | Comma-separated audience filter — required for projected ServiceAccount tokens minted with `--audience`. |

RBAC: the router's ServiceAccount needs `create` on `tokenreviews.authentication.k8s.io`. The `system:auth-delegator` ClusterRole grants exactly this and is the standard pattern (kubelet, metrics-server, kube-state-metrics all use it). The `deploy/rbac.yaml` example wires it.

**Scope of v1.** TokenReview only **authenticates** the caller — it verifies the token belongs to a known principal in the cluster. It does **not** check whether that principal is allowed to access the specific sandbox they named in `X-Sandbox-ID`. Tightening to per-sandbox authorization needs an agreed identity contract on the Sandbox CR (owner label, annotation, or a SubjectAccessReview-style policy) and is tracked as follow-up after KEP-NNNN lands.

### Scoped-token authorizer

Set `--authz-mode=scoped-token` to enable the built-in authorizer that closes exactly the gap called out above, but without requiring the caller to hold a cluster-verifiable K8s credential at all. A scoped token is a small HMAC-SHA256-signed value binding `(namespace, name, exp)` — minted with `authz.MintScopedToken`, wire format `v1.<payload>.<signature>` — and the authorizer both verifies the signature/expiry *and* checks that the token's `(namespace, name)` matches the sandbox actually being addressed. The leading `v1` version lets a future format coexist with outstanding tokens during a rollout, and the signature is domain-separated (MAC'd over a fixed context string) so it can't be cross-verified by any other component sharing the Secret. A token minted for `box-a` gets 403 against `box-b`; there is no TokenReview round-trip and no K8s API access implied by possessing the token. Requests carrying `X-Sandbox-Pod-IP` or `X-Sandbox-UID` are rejected outright in this mode: both override how the proxy picks the dial target *after* authorization (a raw IP, or a UID→IP cache lookup), which would let a token scoped to one sandbox reach a different pod while `X-Sandbox-ID` still names the authorized one. Rejecting them leaves resolution by `(namespace, name)` — exactly the identity the token authorizes — as the only routing path, so the dial target always matches what was authorized. Today that resolution is DNS, so scoped-token mode needs the sandbox reachable by its `(namespace, name)` DNS name (e.g. a headless `Service`), rather than the UID cache fast-path. A `(namespace, name)`-keyed cache name index (added in #1239) resolves the same identity and composes here without weakening the guarantee — it would lift the headless-`Service` requirement for warm-pool sandboxes; whichever of the two lands second should keep this sentence and #1239's wording in sync.

This is the primitive an agent-facing example needs to reproduce the credential-boundary story of `examples/containarium-ssh-sandbox` (agent holds one narrow, single-purpose credential, never a cluster token) using only pieces native to this project — no third-party SSH gateway, no vendor runtime image. `MintScopedToken` is exported so a Sandbox controller (or a test/example harness standing in for one) can mint a token at Sandbox-creation time and hand it to the agent; the router itself never mints, only verifies.

Flags:

| Flag | Default | Notes |
|---|---|---|
| `--authz-mode` | `allow-all` | `allow-all`, `tokenreview`, or `scoped-token`. |
| `--authz-scoped-token-secret-file` | `""` | Path to a file holding the shared HMAC-SHA256 secret. Required when `--authz-mode=scoped-token`; must match whatever minted the tokens (e.g. the same K8s Secret mounted into the controller and the router). At least 32 bytes after whitespace trimming (`authz.MinScopedTokenSecretLen`) — every observed token is an offline brute-force oracle for this secret, so short values are refused at startup. Minter and verifier both trim surrounding whitespace, so a trailing newline in the mounted file is harmless. |

**Follow-up, not in this change.** Nothing here mints tokens automatically at Sandbox creation or rotates the shared secret without a restart — both are natural next steps once a controller-side minting story is agreed, tracked alongside the per-sandbox-authorization follow-up on TokenReview above.

### Browser-session credentials

Both authorizers above check `Authorization: Bearer <token>` by default (via `authz.TokenFromRequest`, which checks that header first) — which is exactly the gap [Browser-facing traffic: path-based routing](#browser-facing-traffic-path-based-routing) already documents for routing, applied to authorization instead. A top-level navigation, an `<iframe src>`, and a WebSocket handshake have no API for setting *any* request header — this isn't specific to `Authorization` being unusual; `fetch`/`XMLHttpRequest` can set it freely, but none of the three request types a browser actually needs for this router (opening an editor tab, embedding a dev-server preview, opening a terminal WebSocket) go through `fetch`. So `--path-routing-prefix` alone gets a browser to the right sandbox, but leaves it unable to present a `tokenreview` or `scoped-token` credential at all — the deployment is stuck on `allow-all` for that traffic.

A cookie is the one credential channel a browser attaches automatically to every request toward an origin — including a WebSocket handshake, which is not subject to the Same-Origin Policy the way a `fetch` is. Set `--authz-cookie-name` (and, to actually get the cookie set, `--authz-cookie-query-param`) to let `TokenFromRequest` additionally accept a credential from a cookie or a URL query parameter, in this order: `Authorization` header, then query parameter, then cookie.

**The bootstrap.** A page cannot set a cookie for another origin itself, so the very first request still needs the credential somewhere a browser *can* put it: the URL. A `GET`/`HEAD`, non-upgrade request that actually matched `--path-routing-prefix` and presented a valid credential via `--authz-cookie-query-param` gets it set as a cookie — scoped by `Path` to exactly `<path-routing-prefix>/<namespace>/<id>/<port>/`, so a browser never attaches sandbox A's cookie to a request under sandbox B's path — and is redirected (`302`) to the same URL with the parameter stripped. (A header-routed request carrying the same query parameter is authorized the same way but never bootstrapped: a cookie scoped to the path-routing shape could never match its URL, so setting one and stripping its only credential would just break the retry.) This collapses the credential's time in the URL, browser history, and any `Referer` a sub-resource load might send, to a single request. Every request after that — including the WebSocket handshake a web IDE's terminal or a dev server's HMR client opens — relies on the cookie instead:

```sh
# First load: token travels once, in the URL.
curl -i 'https://sandbox-router-svc:8443/router/team/my-sandbox/8080/?token=v1.xxx.yyy'
# -> 302, Set-Cookie: sid=v1.xxx.yyy; Path=/router/team/my-sandbox/8080/; HttpOnly; Secure; SameSite=Lax
#    Location: https://sandbox-router-svc:8443/router/team/my-sandbox/8080/

# Every later request, including a WebSocket handshake: the browser
# attaches the cookie on its own, no client-side code involved.
curl -i --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  -H 'Cookie: sid=v1.xxx.yyy' \
  'https://sandbox-router-svc:8443/router/team/my-sandbox/8080/'
# -> 101 Switching Protocols
```

(`https://` here because the cookie is `Secure` by default. Local development over plain HTTP needs `--authz-cookie-insecure` — see the flag below — and `http://` URLs throughout.)

A missing or invalid credential on the bootstrap request behaves exactly like a missing or invalid one anywhere else — 401/403, no `Set-Cookie` — nothing about the redirect is trusted on its own.

**`SameSite` — read this before choosing a value.** `SameSite` compares the registrable domain (eTLD+1), not the origin: a page on `atenea.example.com` embedding an iframe served from `sandboxes.example.com` is a **different origin but the same site**, and `SameSite=Lax` (the default) covers it without requiring HTTPS. Only a genuinely cross-site deployment — integrator and router on unrelated registrable domains — needs `SameSite=None`, and that requires `Secure` (browsers refuse to set a `SameSite=None` cookie without it — incompatible with `--authz-cookie-insecure`); some browsers additionally block a `SameSite=None` cookie outright as third-party, which no flag on this router can work around. Prefer a same-site deployment if you have the choice.

**The Origin allowlist is not optional in this mode — and it does not cover everything.** A cookie is an ambient credential: unlike a header or a query parameter, the browser attaches it to a request that any page — including one an attacker controls — initiated, and this is *not* hypothetical for a WebSocket handshake specifically, which the Same-Origin Policy does not cover the way it covers `fetch` (this is the well-known Cross-Site WebSocket Hijacking class of bug). The router cannot lean on the upstream sandbox to catch a bad `Origin` either, since it strips `Origin` on upgrade requests for code-server's benefit (see [WebSockets and other protocol upgrades](#websockets-and-other-protocol-upgrades) above). So whenever the credential that authorized a request came from the cookie, the router separately checks the request's `Origin` against `--authz-cookie-allowed-origins` — *before* calling into the `Authorizer` at all — and rejects with 403 if it isn't allowed. A same-origin request (scheme AND host both matching the router's own) is always allowed regardless of the list; a request with no `Origin` header at all is let through this specific check, since browsers reliably send `Origin` on exactly the requests it exists to gate (every WebSocket handshake, every genuinely cross-site request) and there is nothing here to inspect otherwise. Header- and query-sourced credentials are exempt from this check entirely — a third-party page cannot forge either.

But **every sandbox behind this router shares the same origin** — they differ only by path, and `Origin` cannot see path. So the allowlist protects against a page on a *different* origin, not against a page served *by one of your own sandboxes*: JavaScript running inside sandbox A can `fetch()`/open a WebSocket to sandbox B's path from the same browser, the request is same-origin by definition, and if that browser also holds a still-valid cookie for sandbox B (the same person has both open), the browser attaches it — cookie `Path` matching is about the request's target URL, not the initiating page's. Nothing in this design closes that gap; the real fix is giving each sandbox its own origin (a distinct hostname or subdomain), which is a bigger architectural change than this opt-in feature makes. Treat every sandbox behind a shared-origin router as at least as trusted as the browser session itself, same as you would for any other same-origin multi-tenant page.

**Same-origin detection needs `--authz-trust-forwarded-proto` behind a TLS-terminating proxy.** The "same-origin request... is always allowed" claim above compares the browser's `Origin` against `scheme://host` derived from the request as *this process* sees it — and the scheme half defaults to `r.TLS != nil`, true only when TLS terminates in the router itself. Any load balancer or Gateway that terminates TLS and forwards plain HTTP to the backend (the standard shape on every major cloud) makes `r.TLS` always nil, so the router always computes its own origin as `http://<host>` while a browser loading the page over HTTPS sends `Origin: https://<host>` — a same-origin request misclassified as cross-origin, rejected unless it also happens to appear in `--authz-cookie-allowed-origins`. Set `--authz-trust-forwarded-proto` to read the scheme from `X-Forwarded-Proto` instead in that case — but only when the router is reachable exclusively through a proxy that sets this header itself and strips any client-supplied one first, since trusting it otherwise lets a client with direct network access forge its own scheme.

**Interaction with the scoped-token authorizer's routing-override check.** `ScopedTokenAuthorizer` rejects any request carrying `X-Sandbox-Pod-IP` or `X-Sandbox-UID` (see above) — for a path-routed request those headers are already inert, since `ParsePathRoute` never populates them, so this fails closed on a browser request that (unusually) sends either header rather than ever creating an actual bypass. Worth knowing about, not something you need to work around.

Flags:

| Flag | Default | Notes |
|---|---|---|
| `--authz-cookie-name` | `""` (disabled) | Name of the session cookie. Requires `--path-routing-prefix` and an `--authz-mode` other than `allow-all`. |
| `--authz-cookie-query-param` | `""` | Name of the URL query parameter that bootstraps the cookie above. Requires `--authz-cookie-name`. |
| `--authz-cookie-samesite` | `lax` | `lax`, `strict`, or `none` — see the `SameSite` note above. |
| `--authz-cookie-insecure` | `false` | Omit `Secure` from the cookie. Local development over plain HTTP only; incompatible with `--authz-cookie-samesite=none`. |
| `--authz-cookie-allowed-origins` | `""` | Comma-separated `scheme://host[:port]` allowlist checked against `Origin` whenever the credential came from the cookie. Same-origin is always allowed regardless. |
| `--authz-trust-forwarded-proto` | `false` | Read the scheme for the same-origin check from the first value of `X-Forwarded-Proto` instead of `r.TLS != nil`. Needed behind any TLS-terminating load balancer or Gateway — see the note below. Only enable it when the router is reachable exclusively through a proxy that sets this header itself and strips any client-supplied one. |

No `Max-Age`/`Expires` is ever set — the cookie lives for the browser session, and the underlying token still carries its own expiry. When it lapses, the browser needs a fresh bootstrap URL; there is no silent renewal.

## TLS / mTLS

The HTTPS listener is opt-in (set `--https-bind-address`). Cert and key are read from `--tls-cert-file` and `--tls-key-file`. Both files are watched: writing a new file (atomic rename, like Kubernetes Secret projection) triggers an automatic reload with no pod restart.

`--mtls-mode` controls client-cert verification:

- `off` — server cert only, no client cert ever required.
- `optional` — if the client presents a cert, it must validate against `--tls-client-ca-file`; if it doesn't, the request proceeds.
- `required` — every connection must present a cert that validates against the CA bundle.

`tls.Config.MinVersion = TLS 1.2`. ALPN advertises `h2` and `http/1.1`.

## Metrics

All metrics live under a private Prometheus registry (no controller-runtime metrics bleed-through) and serve on `--metrics-bind-address`. Per-sandbox `sandbox_id` labels are **intentionally absent** — a cluster can have 10k+ sandboxes, and exposing one series per sandbox would blow up Prometheus storage. Use traces for per-sandbox debugging.

**OTLP push (optional).** Setting `--enable-otel-metrics` additionally pushes every Prometheus series via OTLP gRPC on a periodic interval. The bridge reads from the same Prometheus registry, so pull and push consumers see the same data — no double-instrumentation. The OTLP endpoint, headers, compression, and TLS are configured via the standard `OTEL_EXPORTER_OTLP_ENDPOINT` env var (or the metric-specific `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`).

| Name | Type | Labels |
|---|---|---|
| `sandbox_router_requests_total` | counter | `method`, `code`, `sandbox_namespace` |
| `sandbox_router_request_duration_seconds` | histogram | `method`, `code`, `sandbox_namespace` |
| `sandbox_router_inflight_requests` | gauge | — |
| `sandbox_router_upstream_errors_total` | counter | `sandbox_namespace`, `reason` (`dial` / `timeout` / `tls` / `eof` / `other`) |
| `sandbox_router_upstream_retries_total` | counter | `sandbox_namespace` |
| `sandbox_router_cache_invalidations_total` | counter | `sandbox_namespace` (KEP-NNNN active invalidation: bumped when the proxy evicts a cached IP after a dial failure) |
| `sandbox_router_authz_decisions_total` | counter | `sandbox_namespace`, `decision` (`allow` / `deny`) |
| `sandbox_router_cert_reloads_total` | counter | `outcome` (`success` / `failure`) |
| `sandbox_router_build_info` | gauge (const labels) | `git_version`, `git_commit`, `build_date`, `go_version`, `compiler`, `platform` |

## Tracing

Setting `--enable-tracing` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) initializes the OTLP gRPC exporter through `internal/metrics.SetupOTel`. Every request gets a server span (`HTTP <method>`) with attributes `http.method`, `http.target`, `http.status_code`, `sandbox.id`, `sandbox.namespace`. W3C trace context is extracted from inbound headers and re-injected into the outbound request so the sandbox sees a continuation of the trace.

When tracing is on, every log line emitted by the access log middleware and the proxy error handler includes `trace_id` and `span_id` fields, so you can jump straight from a span to its log lines.

## Access logging

One structured log line per request is emitted to the `sandbox-router.access` logger:

```json
{"level":"info","logger":"sandbox-router.access","msg":"request",
 "method":"POST","path":"/api/v1/run","status":200,
 "duration_ms":42,"client_ip":"10.0.1.5","sandbox_id":"abc-123",
 "sandbox_namespace":"team-a","bytes_out":1024,
 "user_agent":"agent-sandbox-go/0.1",
 "trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}
```

`/healthz`, `/readyz`, and `/metrics` are excluded so high-frequency probes don't drown the signal. Set `--access-log=false` to disable entirely.

**Client IP**: today we report `r.RemoteAddr` only. Supporting `X-Forwarded-For` requires configuring a trusted proxy chain — deferred to keep v1 small. Behind an L7 LB that rewrites the source, treat the LB's own access logs as authoritative for the client IP.

## Configuration file

In addition to flags and env vars, the router accepts a YAML config file via `--config FILE` or the `SANDBOX_ROUTER_CONFIG` environment variable. Keys are kebab-case and match flag names one-to-one. Unknown keys cause startup failure (no silent typos).

```yaml
# /etc/sandbox-router/config.yaml
http-bind-address: ":8080"
https-bind-address: ":8443"
tls-cert-file: "/tls/tls.crt"
tls-key-file: "/tls/tls.key"
tls-client-ca-file: "/tls/ca.crt"
mtls-mode: "required"
cluster-domain: "cluster.local"
proxy-timeout: "180s"
upstream-max-retries: 3
enable-tracing: true
enable-otel-metrics: true
```

**Precedence (highest wins):** CLI flags > file > env vars > built-in defaults.

## Deployment

Example K8s manifests live in [`deploy/`](deploy/) — Deployment, Service, PodDisruptionBudget, NetworkPolicy, plus a README that walks through what to tighten before production.

```sh
kubectl apply -f sandbox-router/deploy/
```

## Scaling guidance

A locally-hosted load test lives at [`dev/load-test/router/`](../dev/load-test/router/). Drives synthetic load through an in-process router into a no-op backend so capacity numbers can be captured without a cluster.

```sh
go run ./dev/load-test/router --in-process --concurrency=50 --duration=30s
```

Reference numbers from a single-binary, single-machine run (Linux x86_64, Go 1.26, loopback) — these are **upper bounds**; real cluster numbers will be lower due to network and TLS overhead:

| Concurrency | Throughput | p50 | p95 | p99 |
|---:|---:|---:|---:|---:|
| 10 | ~5,000 req/s | 1.2 ms | 4.8 ms | 7.8 ms |
| 50 | ~5,800 req/s | 6.4 ms | 15.2 ms | 22.0 ms |
| 200 | ~4,500 req/s | 34.3 ms | 72.5 ms | 99.9 ms |
| 50 with 4 KB POST body | ~3,600 req/s | 10.1 ms | 24.7 ms | 35.4 ms |

**How to size:**
- **CPU-bound at high RPS.** Start with the deployment's default 250m request / 2 CPU limit and scale horizontally based on `sandbox_router_inflight_requests` or CPU utilization.
- **Two replicas is the HA floor**, not a capacity recommendation. Expect a single replica to handle low-thousands req/s on modest hardware; benchmark in your cluster before committing.
- **TLS adds overhead.** Plain HTTP numbers above don't include TLS handshakes. Add ~10-20% latency for new TLS connections; reused connections via HTTP keep-alive amortize it.
- **Per-sandbox cardinality doesn't affect router perf.** The router is namespace-aware (for metric labels) but otherwise stateless per sandbox — handling 100 vs 10,000 sandboxes is the same cost.

## When to consider Envoy instead

The router today is a small, focused reverse proxy with a header-driven routing rule. Several features you'd want for enterprise deployments — rate limiting, circuit breaker / outlier detection, JWT/OIDC auth, WAF, advanced LB algorithms, dynamic config reload — are things Envoy ships out of the box. If you find yourself wanting those, the hybrid pattern is usually right:

- **Envoy as the edge** (TLS, mTLS, rate limit, circuit breaker, observability, JWT).
- **This router as a backend** behind Envoy, owning only the sandbox-specific routing logic (header parsing → DNS construction → per-sandbox authz when that lands).

The Python router's architecture already has this shape — a Kubernetes Gateway API `Gateway` in front of the router. That Gateway can be backed by Istio, Envoy, a cloud-managed LB (GKE, ALB, etc.), or any conformant Gateway API controller — and you avoid rebuilding generic L7 concerns in Go.

If you stay all-Go, the access log, OTel signals, hot-reloading certs, and retry/backoff give you the operational basics; the gaps (rate limit, circuit breaker, etc.) are tracked as future work.

## Building

```sh
make build-sandbox-router         # writes bin/sandbox-router
go test ./sandbox-router/...                  # unit tests
go test -tags=integration ./sandbox-router/...# integration tests (TLS handshakes, real backends)
```

The Docker image is built by `dev/tools/push-images`, which is patched to use the repo root as the build context for `sandbox-router/Dockerfile` and to name the image `sandbox-router-go` (to avoid colliding with the Python router's `sandbox-router` image). Final stage is `gcr.io/distroless/static:nonroot`.

```sh
./dev/tools/push-images --images sandbox-router-go --image-tag dev
```

## Compatibility with the Python router

This Go router is a **drop-in replacement** for the protocol contract: same `sandbox-router-svc:8080` Service name, same headers, same JSON error shape, same `PROXY_TIMEOUT_SECONDS` and `CLUSTER_DOMAIN` env-var support. The Python router's source is retained at `clients/python/agentic-sandbox-client/sandbox-router/` for reference until deprecation is formalized; the Docker images, however, are distinct (`sandbox-router` vs `sandbox-router-go`).

What's new beyond the Python router:

- TLS / mTLS termination with hot-reload
- Prometheus metrics and OpenTelemetry tracing
- Configurable dial-retry with backoff
- Graceful shutdown (readiness flip, parallel drain, bounded timeout)
- Strict request-body size limit (`--max-request-body-bytes`)
- Built as a multi-arch distroless static image
