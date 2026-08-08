# Management API

## Problem

The original sandbox SDKs require a Kubernetes client and a valid `kubeconfig` to create and delete `SandboxClaim` objects. This has two practical consequences:

1. **Every language needs a full K8s client library.** Writing an SDK in TypeScript, Java, or C# means pulling in a heavy K8s client dependency and wiring up cluster auth before you can do anything.
2. **Callers must be in-cluster or have direct API server access.** AI agents running in a different cluster, or outside the cluster entirely, cannot reach the K8s API server without additional network plumbing (VPN, konnectivity, etc.).

The sandbox-router already runs inside the cluster and already speaks to the K8s API server. It is also the component that handles all proxy traffic to sandbox pods.

## Solution

This package extends the router with a thin REST management layer at `/v1/sandboxes`. It translates plain HTTP calls into `SandboxClaim` CRUD operations, so callers no longer need a K8s client or kubeconfig — only a URL.

```
Client (any language)          sandbox-router                 Kubernetes
        │                           │                              │
        │  POST /v1/sandboxes       │                              │
        │──────────────────────────>│  Create SandboxClaim CR      │
        │                           │─────────────────────────────>│
        │  {id, status, headers}    │                              │
        │<──────────────────────────│                              │
        │                           │                              │
        │  GET /sandbox-path        │                              │
        │  X-Sandbox-ID: <id>       │  proxy to sandbox pod        │
        │──────────────────────────>│─────────────────────────────>│
```

The `connection` field in every response carries the exact headers the caller must set when sending proxied requests (`X-Sandbox-ID`, `X-Sandbox-Namespace`). SDKs never hard-code header names.

Exposing the router via an ingress or gateway API is now sufficient to give clients in any cluster — or outside the cluster — full sandbox lifecycle management.

## Idempotency key

`POST /v1/sandboxes` is not naturally idempotent: the K8s API assigns the `SandboxClaim` name server-side (via `GenerateName`). If the network drops the response after the claim is created, the caller has no way to recover the ID.

The `idempotencyKey` field solves this. The caller generates a UUID before sending the request and includes it in the body:

```json
{ "warmPool": "ai-pool", "idempotencyKey": "550e8400-e29b-41d4-a716-446655440000" }
```

The router stores the key as a label on the `SandboxClaim` (`sandbox.intapp.com/idempotency-key`). On every subsequent `POST` with the same key and namespace, the router checks for an existing claim carrying that label and returns it instead of creating a new one. The operation is safe to retry unconditionally.

Keys are scoped to a namespace — the same key in two different namespaces produces two distinct claims.

Keys must be valid Kubernetes label values (≤ 63 characters, alphanumeric, `-`, `.`, `_`, starting and ending with alphanumeric). A UUID always satisfies this constraint.

## Design decisions

- **Zero changes to the proxy path.** Management routes are registered before the catch-all proxy route and live entirely in this package. The proxy, cache, and authz layers are untouched.
- **Feature-gated.** The management API is off by default and enabled with `--management-api`. This keeps existing deployments unaffected.
- **No new network surface for proxy traffic.** Management and proxy traffic share the same port and listener. Cross-cluster access is handled by exposing the router through an existing ingress, not by opening a new port.
