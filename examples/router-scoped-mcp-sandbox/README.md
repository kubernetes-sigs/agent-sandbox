# Router-Scoped MCP Sandbox

An example of running an MCP server inside a Sandbox and reaching it
entirely through `sandbox-router`'s `--authz-mode=scoped-token`, so the
agent's only credential is a small signed token bound to *one* sandbox —
never a Kubernetes Bearer token, and never a third-party gateway or vendor
runtime image.

This is the vendor-neutral sibling of
[`examples/containarium-ssh-sandbox`](../containarium-ssh-sandbox): that
example proves the same "agent holds a narrow, single-purpose credential"
property using an SSH key, dropbear, and Containarium's `agent-box` image.
Here the identical property is reproduced with nothing but pieces that ship
with `agent-sandbox` itself — the router's `Authorizer` plugin point
(`sandbox-router/authz`) and a single Go helper standing in for the
Sandbox controller.

## How it works

```text
┌──────────── host ────────────┐        ┌──────── sandbox-router ────────┐        ┌────── sandbox pod ──────┐
│                              │        │                                 │        │                          │
│  mint-token (stand-in for    │        │  --authz-mode=scoped-token      │        │  mcp_server.py           │
│  the Sandbox controller)     │        │    verifies (ns, name, exp)      │        │    (streamable-http)     │
│    └─ produces a token bound │        │    against the request's        │  HTTP  │      /workspace ──► PVC │
│       to (ns="default",      │        │    X-Sandbox-Namespace/-ID —    │ ─────► │                          │
│       name="box-a")          │        │    NOT against the K8s API       │        │                          │
│                              │        │                                 │        │                          │
│  client.py                   │ ─────► │  (no kubeconfig, no TokenReview, │        │                          │
│    Authorization: Bearer …   │        │   no third-party gateway)        │        │                          │
└──────────────────────────────┘        └─────────────────────────────────┘        └──────────────────────────┘
```

Compare the three examples' credential model side by side:

| Example | Agent's credential | Verified by |
|---|---|---|
| `mcp-server-sandbox` | A K8s Bearer token good enough to `kubectl exec` | kube-apiserver RBAC |
| `containarium-ssh-sandbox` | An SSH key, forced into one command | sshpiper + dropbear (third-party) |
| **this example** | A signed token bound to one `(namespace, name)` | `sandbox-router`'s own `ScopedTokenAuthorizer` |

A token minted for `box-a` carries no K8s semantics at all — it can't be
presented to the API server for anything — and the router rejects it with
403 the moment it's used against `box-b`. That's the property
`TokenReviewAuthorizer` explicitly does not provide today (see its
docstring in `sandbox-router/authz/tokenreview.go`): any authenticated
Bearer token can address *any* Sandbox in the cluster.

## Files

| File | Role |
|---|---|
| [`sandbox.yaml`](sandbox.yaml) | `Sandbox` named `box-a`. `automountServiceAccountToken: false` — the pod has no K8s credential to begin with. Runs the MCP server on port 8000, non-root, dropped caps, `RuntimeDefault` seccomp. |
| [`sandbox-box-b.yaml`](sandbox-box-b.yaml) | Identical second `Sandbox`, `box-b` — the target for the negative (scoping) test. |
| [`Dockerfile`](Dockerfile) | `python:3.11-slim` + `pip install mcp` + `mcp_server.py`. Runs continuously (unlike `mcp-server-sandbox`'s per-`kubectl-exec` process). |
| [`mcp_server.py`](mcp_server.py) | MCP server over the `streamable-http` transport. Same `list_blobs` / `write_random_blob` / `read_blob` tools as `mcp-server-sandbox`, forked as a starting point. |
| [`client.py`](client.py) | Host-side MCP client. Talks to the router over plain HTTP with the four `X-Sandbox-*` headers plus `Authorization: Bearer <token>` — no `kubectl`, no `ssh`, anywhere in this file. |
| [`mint-token/main.go`](mint-token/main.go) | CLI wrapping `authz.MintScopedToken`. Stands in for the Sandbox controller, which is where minting belongs in production — see "What this example does *not* solve" below. |
| [`run-test-kind.sh`](run-test-kind.sh) | Builds the router + MCP images, deploys `sandbox-router` with `--authz-mode=scoped-token`, and runs the three checks described below. |

## Prerequisites

1. A Kubernetes cluster with the [Agent Sandbox controller](../../README.md#installation) installed (core CRDs only).
2. `kubectl`, `docker`, `envsubst`, `go` (module-local — no separate install needed inside this repo), and Python 3.10+ on the host.
3. This example deploys its own `sandbox-router` — the scoped-token authorizer isn't in any published router image yet, so `run-test-kind.sh` builds one from source.

## Run it (local cluster, e.g. Kind)

```bash
./run-test-kind.sh
```

This builds and loads both images, deploys `sandbox-router` configured
with `--authz-mode=scoped-token`, applies both Sandboxes, mints a token
scoped to `box-a`, and runs three checks:

```text
=== Test 1: box-a's token against box-a — expect success ===
[host] write_random_blob('random.bin', 256) -> {'path': '/workspace/random.bin', 'bytes_written': 256, 'sha256': '...'}
[host] read_blob('random.bin') -> {'path': '/workspace/random.bin', 'size_bytes': 256, 'sha256': '...'}
[host] OK — round-trip sha256 matches: ...

=== Test 2: box-a's token against box-b — expect rejection (scoping) ===
[host] OK — request against 'box-b' was rejected as expected: ...

=== Test 3: a forged token against box-a — expect rejection ===
[host] OK — request against 'box-a' was rejected as expected: ...

All checks passed.
```

Tear-down is automatic (the script's `EXIT` trap deletes both Sandboxes,
the router deployment, and the shared secret).

## What this example does *not* solve

- **Automatic minting at Sandbox-creation time.** `mint-token` is a
  standalone CLI so the pattern can be exercised today; wiring minting
  into the Sandbox controller (and deciding how the token reaches the
  agent — status field? a controller-managed Secret?) is a real design
  question, intentionally left open here.
- **Secret rotation.** One static HMAC secret, matching how `sandbox-router`
  started with a single TLS cert before hot-reload was added. Multi-key
  verification during rotation is a follow-up.
- **A published router image with this authorizer.** Until the
  `scoped-token` authorizer lands upstream, `run-test-kind.sh` builds
  `sandbox-router` from source.

## References

- [`sandbox-router/README.md`](../../sandbox-router/README.md) — the
  "Scoped-token authorizer" section documents the flag surface and the
  `Authorizer` contract this builds on.
- [`examples/containarium-ssh-sandbox`](../containarium-ssh-sandbox) — the
  SSH/vendor-image version of the same credential-boundary pattern.
- [`examples/mcp-server-sandbox`](../mcp-server-sandbox) — the
  `kubectl exec` baseline this and `containarium-ssh-sandbox` both improve
  on.
