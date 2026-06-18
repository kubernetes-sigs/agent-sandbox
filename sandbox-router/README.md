# sandbox-router

A top-level component implementing [KEP-NNNN](https://github.com/kubernetes-sigs/agent-sandbox/pull/758) (Go-Based Sandbox Router with Pod IP Mapping) using **Envoy + ext_proc** rather than a from-scratch Go reverse proxy.

The KEP's three core mechanisms — informer-backed UID→PodIP map, direct Pod IP dispatch, drift-handling — are still implemented in Go. Everything else (TLS termination, mTLS, rate limiting, circuit breaker, access logs, tracing, retries) is delegated to Envoy.

## Architecture

```text
client ──► Envoy listener (:8080)
            │
            ├── HTTP filter: ext_proc
            │     │
            │     │  gRPC ProcessingRequest (RequestHeaders)
            │     ▼
            │   ext-proc-sandbox-router service
            │     │  - K8s Pod informer (labeled sandbox-name-hash)
            │     │  - In-memory UID → PodIP map
            │     │  - Reads X-Sandbox-UID + X-Sandbox-ID etc.
            │     │  - Returns header mutation:
            │     │    x-envoy-original-dst-host = <PodIP>:<port>
            │     │    (or DNS form on cache miss)
            │     ▼
            │   gRPC ProcessingResponse (HeaderMutation + ClearRouteCache)
            │
            └── HTTP filter: router → ORIGINAL_DST cluster
                                       (reads x-envoy-original-dst-host)
                ─────► sandbox Pod
```

**What Envoy owns:** TLS termination, mTLS, rate limiting (Envoy's `local_ratelimit` filter), circuit breaker / outlier detection on per-Pod-IP endpoints, access logs (Envoy access log), distributed tracing (OTel native), retries, hedged retries, JWT auth, WAF — everything Envoy is good at, available without further code.

**What the Go ext-proc service owns:** the single piece of K8s-aware state. ~300 lines of Go.

## Request contract

Envoy expects these headers from the client (preserved from the Python router for compatibility):

| Header | Required? | Default | Notes |
|---|---|---|---|
| `X-Sandbox-ID` | yes | — | Sandbox name. Used for DNS-form fallback when UID is missing or uncached. Must be a valid DNS-1123 label (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 chars). |
| `X-Sandbox-UID` | recommended | — | Sandbox CR UID. Primary key for cache lookup; non-guessable. |
| `X-Sandbox-Namespace` | no | `default` | Same DNS-1123 label check as ID. |
| `X-Sandbox-Port` | no | `8888` | Integer in `[1, 65535]`. |

DNS-label validation on both ID and namespace prevents DNS injection (e.g. `foo.evil.com` interpolating extra components into the upstream FQDN) and traversal-style inputs (e.g. `foo/bar`). Matches the Python router's `_is_valid_dns_label` check.

Routing precedence:

1. If `X-Sandbox-UID` is set and the cache has an entry → upstream is `<PodIP>:<port>` (fast path).
2. Otherwise → upstream is `<X-Sandbox-ID>.<namespace>.svc.<cluster-domain>:<port>` (DNS fallback; works without UID, slower).
3. If `X-Sandbox-ID` is missing → 400 `{"detail":"X-Sandbox-ID header is required."}`.

The ext_proc handler returns Python-router-compatible JSON error bodies (`{"detail":"..."}`) for validation failures so existing clients keep working.

### Headers stripped before forwarding

The `HeaderMutation` returned to Envoy always removes:

- **`Authorization`** — identity-checking authorizers in front of ext_proc (TokenReview, JWT, etc.) consume the bearer credential. Forwarding it to the sandbox would let the sandbox impersonate the caller against the K8s API or any other Bearer-protected service. Matches the Python router, which strips `Authorization` right next to `Host`.
- **`x-envoy-original-dst-host`** on the listener (defense in depth) — an `envoy.filters.http.header_mutation` filter runs *before* ext_proc and removes any client-supplied value, so ext_proc is provably the only writer of that header. Without this, a future route that disables ext_proc and uses the ORIGINAL_DST cluster would dispatch to whatever the client asked for.
- **`Origin` on upgrade requests** — see the WebSockets section below.

### WebSockets and X-Forwarded-* headers

Two small carve-outs make browser-facing backends (vscode-server, Jupyter, anything that holds an Origin/Host CSRF check) work without changes:

- **`Origin` is stripped on upgrade requests.** When the inbound request carries `Connection: Upgrade` + a non-empty `Upgrade:` header, the ext_proc handler returns a `HeaderMutation` with `RemoveHeaders: ["origin"]` alongside the dst-host set. The reason: vscode-server and similar backends reject the WebSocket upgrade with a `1006` close when the client-supplied `Origin` doesn't match the backend's own `Host`. Normal HTTP traffic preserves `Origin` so CORS preflights stay intact.
- **`X-Forwarded-Host` is set in the Envoy config** via `request_headers_to_add` (the value is `%REQ(:AUTHORITY)%`). `X-Forwarded-For` and `X-Forwarded-Proto` come for free from `use_remote_address: true`. Browser-facing backends use these to construct correct self-links and redirects.

## Components

| Path | Purpose |
|---|---|
| `cmd/ext-proc/main.go` | Binary entrypoint. Wires K8s client + cache + ext_proc server + gRPC health. |
| `internal/cache/` | `SharedInformer` on Pods filtered by `agents.x-k8s.io/sandbox-name-hash`; thread-safe `UID → Entry` map; `Get()` returns `(Entry, bool)`. |
| `internal/extproc/` | Envoy ext_proc v3 `ExternalProcessor` server. Reads request headers, looks up via cache, sets `x-envoy-original-dst-host`. |
| `deploy/envoy/` | ConfigMap with the Envoy config, Deployment, Service named `sandbox-router-svc`. |
| `deploy/ext-proc/` | Deployment (3 replicas), headless Service, ServiceAccount, ClusterRole (`get`/`list`/`watch` on Pods), PDB. |
| `Dockerfile` | Multi-stage distroless static. |

## Flags

`./bin/ext-proc --help` lists the full set. Highlights:

| Flag | Default | Purpose |
|---|---|---|
| `--listen-address` | `:50051` | gRPC bind address for ext_proc. |
| `--namespace` | `""` (cluster-wide) | Restrict informer to a single namespace. |
| `--cluster-domain` | `cluster.local` | Suffix for the DNS-form fallback. |
| `--informer-sync-timeout` | `2m` | Max wait for initial Pod list before /readyz stays failing. |
| `--shutdown-timeout` | `30s` | Drain budget for in-flight ext_proc streams on SIGTERM. |
| `--kubeconfig` | unset | Out-of-cluster path; honors `KUBECONFIG`. In-cluster otherwise. |

## Build

```sh
make build-sandbox-router-ext-proc       # writes bin/ext-proc
go test ./sandbox-router/...             # unit tests
```

## Load testing

A standalone harness at [`cmd/load-test`](cmd/load-test/) runs the real `Server` + real `Cache` (backed by a fake K8s client) in-process and drives concurrent gRPC clients against it. Useful for characterizing the per-replica ceiling without a kind cluster.

```sh
go run ./sandbox-router/cmd/load-test \
    --sandboxes=5000 --churn-rate=100 \
    --clients=64 --duration=30s --cache-hit-pct=80
```

Reference numbers on a single development machine (Linux x86_64, Go 1.26, TCP loopback) — these are **upper bounds per replica**; real deployments add Envoy + network overhead:

| Pool size | Clients | Churn ops/s | Throughput | p50 | p99 | Errors |
|---:|---:|---:|---:|---:|---:|---:|
| 5,000 | 64 | 0 | 38,700 req/s | 1.4 ms | 6.0 ms | 0 |
| 5,000 | 64 | 100 | 35,000 req/s | 1.5 ms | 7.3 ms | 0 |
| 10,000 | 200 | 500 | 37,200 req/s | 4.2 ms | 21.1 ms | 0 |
| 5,000 | 64 | 1,000 | 30,300 req/s | 1.6 ms | 7.1 ms | 0 |
| 1,000 | 256 | 0 | 45,800 req/s | 4.6 ms | 21.1 ms | 0 |

The bottleneck at these numbers is the gRPC machinery itself (TCP loopback + protobuf encode/decode), not the cache lookup — even at 1,000 churn ops/s (≈ 40 % of the pool replaced per minute) throughput only drops ~20 % and there are no errors. Cache size accurately tracks the target throughout.

## Deploy

```sh
kubectl apply -f sandbox-router/deploy/ext-proc/
kubectl apply -f sandbox-router/deploy/envoy/

# Wait for both Deployments to be ready
kubectl rollout status deploy/ext-proc-sandbox-router
kubectl rollout status deploy/sandbox-router-envoy

# Verify Envoy /healthz (bypasses ext_proc)
kubectl run -it --rm curl --image=curlimages/curl --restart=Never -- \
    curl -sS http://sandbox-router-svc.default.svc.cluster.local:8080/healthz
# expected: {"status":"ok"}
```

## How requests flow (end-to-end)

1. Client sends `GET / HTTP/1.1\r\nX-Sandbox-ID: abc\r\nX-Sandbox-UID: <uuid>\r\n…` to `sandbox-router-svc:8080`.
2. Envoy's HCM ext_proc filter sends a `ProcessingRequest{RequestHeaders}` gRPC message to the ext_proc service.
3. ext_proc service reads the headers, looks up the UID in its in-memory cache, and replies with a `HeadersResponse{HeaderMutation}` setting `x-envoy-original-dst-host: 10.0.4.42:8888` and `ClearRouteCache: true`.
4. Envoy re-runs route matching with the mutated header; the `sandbox_upstream` ORIGINAL_DST cluster reads `x-envoy-original-dst-host` and dispatches the request directly to `10.0.4.42:8888`.
5. Sandbox Pod processes the request; response streams back through Envoy to the client.

Total per-request overhead: one gRPC round-trip to the ext_proc service (~sub-ms in-cluster) plus one TCP connection to the Pod IP (or a reused one from Envoy's pool).

## Drift handling

Three mechanisms compose to keep stale entries from causing user-visible errors:

| Drift case | Mitigation |
|---|---|
| Sandbox Pod deleted but ext_proc cache lags | Envoy ORIGINAL_DST outlier detection ejects the dead IP after 3 consecutive 5xx; next dispatch falls through to a fresh resolve. |
| Sandbox Pod NotReady mid-request | ext_proc cache removes the entry on the NotReady UPDATE event; subsequent requests fall back to DNS or get 502. |
| ext_proc replica with stale cache (just started, informer not synced) | gRPC health check stays NOT_SERVING until `informer.HasSynced()`; Envoy routes around. |
| Sandbox UID never seen by cache (informer missed event) | DNS form fallback (`<sandbox-id>.<namespace>.svc.<cluster-domain>`) routes through standard K8s networking. |
| ext_proc service entirely down | Envoy filter has `failure_mode_allow: false`; client gets 503 immediately rather than mis-routing. |

## Comparison with the from-scratch Go router

| Concern | From-scratch Go router | Envoy + ext_proc (this) |
|---|---|---|
| TLS / mTLS | Custom implementation (~400 LOC + hot reload) | Envoy native |
| Rate limit | Would need to add | Envoy `local_ratelimit` filter |
| Circuit breaker | Would need to add | Envoy outlier detection (built in) |
| Access logs | Custom middleware (~150 LOC) | Envoy access log (config-only) |
| Tracing | Custom OTel middleware (~100 LOC) | Envoy OTel native |
| Retries | Custom retry transport (~150 LOC + tests) | Envoy retry policy + hedged retries |
| Custom Go LOC | ~5,000 | ~300 |
| K8s API surface | Optional informer | Required informer (same code) |
| Operational footprint | One Go binary | Envoy + Go service (two Deployments) |

## Not in this prototype

These were intentionally deferred to keep the prototype small. None blocks the core routing path:

- **TokenReview auth.** The KEP's middleware example. Drops in as an `ext_authz` filter pointing at an authz extension of this service (or a sibling).
- **TLS on the ext_proc gRPC channel.** Envoy↔ext_proc is currently plaintext-on-cluster. Add `transport_socket: tls` for production.
- **Prometheus metrics on the ext_proc service.** Envoy's own stats cover most of it; the Go service can expose its own `/metrics` later for cache size / sync state.
- **Hardening of the example manifests.** No NetworkPolicy, no HPA, replica counts are minimum. See the `deploy/` files for what to tighten.
- **Load test capture.** Should add once the prototype is running against kind.

## Why the Sandbox UID isn't a Pod label

The KEP says "Sandbox CRD UID (read from Pod labels)" but in practice the controller does NOT stamp the UID on the Pod as a label today. The only labels on a sandbox Pod are `agents.x-k8s.io/sandbox-name-hash` and any propagated user labels from `PodTemplate.Labels`.

The Sandbox UID IS available on `Pod.metadata.ownerReferences[0].UID` (set by `ctrl.SetControllerReference()` in `controllers/sandbox_controller.go`). We extract it from there, filtering on `Kind=Sandbox` and `APIGroup=agents.x-k8s.io`. No second informer needed.

If we want to allow direct label-based lookup (which would let other controllers query "what's the UID of sandbox X" without walking ownerReferences), that's a follow-up — small controller change to add an `agents.x-k8s.io/sandbox-uid` label on the Pod.
