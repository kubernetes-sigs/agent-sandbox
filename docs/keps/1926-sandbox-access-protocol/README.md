# KEP-1926: Sandbox Access Protocol

## Summary

Add a versioned operator-facing contract, the Sandbox Access Protocol (SAP), for discovering sandboxes and attaching a bidirectional session. SAP sits above the in-sandbox runtime defined by KEP-539.2 and below any one product UI. A client lists the sandboxes it may see and opens a terminal or, later, a control stream. It does not create sandboxes, choose the process inside an attach, or replace `sandboxd`.

## Motivation

Operator tools each build a private way into a sandbox: an SSH config, a local socket, a Tailscale enrollment, or a one-time pairing URL. The sandbox is already a Sandbox object, and `sandbox-router` is already the authenticated HTTP entry point that routes with `X-Sandbox-*` headers, including WebSocket upgrades.

KEP-539.2 standardizes the runtime interface inside the pod (`ProcessService` on gRPC and `FilesystemService` on HTTP). That interface is how an SDK runs a process and touches files. It does not say which sandboxes an operator may see, or how a UI opens a session when the tool is a terminal workspace rather than an SDK. SAP is that missing catalog and session contract. Herdr, Roamgate, T3, Orca, and later clients can implement it once and keep their own UX.

The current Go `sandbox-router` accepts HTTP/2 from clients but disables HTTP/2 toward upstreams, so it cannot carry `sandboxd`'s gRPC `ProcessService`. SAP does not depend on closing that gap. Attach is a WebSocket at the access plane. An implementation may proxy that socket onward, or bridge it through an API-server exec stream when a runtime is only reachable that way.

### User Stories (Optional)

* As an operator, I see a sandbox in my client as soon as its Sandbox exists, without pairing or writing SSH config.
* As a terminal client, I attach one streaming session and send input and window size. I do not send a command line.
* As an agent-control client, I can later attach a structured channel on the same discovery record. Clients that only need a terminal ignore it.
* As a cluster admin, I keep TokenReview, mTLS, or whatever the router already enforces. SAP does not add a second public entry point.

### Goals

* Define a small v1alpha1 discovery and attach contract.
* Reuse `sandbox-router` authentication and `X-Sandbox-*` routing.
* Leave the Sandbox CRD lifecycle and KEP-539.2 conformance unchanged.
* Allow more than one attach adapter. WebSocket-to-upstream and WebSocket-to-exec are both valid.

### Non-Goals

* Replacing `sandboxd` or proxying its gRPC API.
* Standardizing worktree operations, annotations, file transfer, or MCP in the minimum set.
* Choosing a product UI, terminal emulator, or agent runtime.
* Changing NetworkPolicy or L7 egress behavior.

## Proposal

### High-Level Design

```text
Operator client (terminal UI, agent UI, browser)
        │  SAP: list, get, attach WebSocket
        ▼
sandbox-router or a thin companion
        │  existing auth; X-Sandbox-* toward the sandbox
        ▼
Sandbox pod
  ├── sandboxd (KEP-539.2), when the adapter can reach it
  └── optional tool daemon (Herdr, t3 serve, …)
```

The router, or a companion beside it, watches Sandbox objects for discovery and enforces the same authorization it already uses for proxied requests. List results are the sandboxes that authorization allows, not every object in the cluster. Each item carries the DNS-1123 name used as `X-Sandbox-ID`, the namespace, labels, phase, and a coarse activity value: `idle`, `working`, `waiting`, or `blocked`.

Attach upgrades to WebSocket subprotocol `sap.v1`. Frames are JSON text. Byte payloads are standard base64, at most 32 KiB. The first server frame is status. The client sends `input`, `resize`, and `close`. The server sends `output`, `status`, `error`, and `exit`. The access plane selects the process for the terminal channel. A client-supplied command is not part of the contract. A second channel name, `control`, is reserved and is advertised only when an implementation has one.

Activity in the minimum set may be derived from the Sandbox phase plus the access plane's own observation of the session. Semantic agent state (`waiting` on a human, for example) is an extension a tool daemon can report later. Clients must tolerate `waiting` without requiring it.

Where the router can open an HTTP connection to the sandbox, it proxies the WebSocket with the existing `X-Sandbox-*` headers and does not accept a caller-supplied pod IP beyond the router's current checks. Where the runtime listens only on loopback and the dataplane cannot reach it, an adapter may bridge the same WebSocket to an authenticated API-server exec stream. Both adapters speak SAP to the client. A reference plane that uses the exec bridge for project sandboxes is in [agent-project-controller#13](https://github.com/SkippySteve/agent-project-controller/pull/13). That repository is not part of this KEP's implementation requirement.

#### API Changes

No Sandbox CRD change is required for the minimum set.

HTTP, version `sandboxaccess.agents.x-k8s.io/v1alpha1`:

* `GET /sap/v1/sandboxes` returns a `SandboxList`.
* `GET /sap/v1/sandboxes/{namespace}/{name}` returns one `Sandbox`.
* `GET /sap/v1/sandboxes/{namespace}/{name}/attach?channel=terminal` upgrades to WebSocket subprotocol `sap.v1`.

`namespace` and `name` are DNS-1123 labels. `id` equals `name` so it can be copied to `X-Sandbox-ID`. Phase is `Pending`, `Ready`, `Paused`, `Blocked`, or `Terminating`. Attach is offered only when the implementation considers the sandbox ready and the requested channel is advertised. A busy terminal attach returns 409. Unknown query parameters, including `command`, are rejected.

This KEP does not add those routes to the core apiserver. They belong on the router or its companion.

#### Implementation Guidance

1. Add the OpenAPI document for the HTTP and WebSocket frames next to the router.
2. Implement List and Get from the router's existing Sandbox cache, filtered by the router's authorizer. Unsupported authorizers fail closed.
3. Implement Attach as a WebSocket upgrade that selects the upstream with the same header validation the router already uses for proxying.
4. Document the exec-stream adapter as an allowed implementation for runtimes the router cannot dial. Do not make it the only adapter.
5. Publish thin Go and TypeScript clients after the OpenAPI is reviewed. The external prototype can be mined for tests; it should not be vendored wholesale.
6. Conformance for v1alpha1 is: unauthorized list fails, a sandbox the caller may see appears in the list, attach of an unknown sandbox fails, attach does not accept a command, and a terminal session carries input bytes to output bytes.

## Scalability

Discovery reads a bounded cache of Sandbox objects. It does not list pods on each request. Response size is one record per visible sandbox, not logs or file contents. Attach holds one WebSocket per session. An implementation may limit concurrent attaches per sandbox; the reference behavior is one terminal attach at a time. The router already fans in to many sandboxes, and SAP does not add a second watch per client beyond the client's own reconnect.

## Alternatives (Optional)

* **Keep per-tool tunnels.** Each client continues to invent pairing, SSH, or a local socket. That preserves today's behavior and does not give a new tool a sandbox it can find.
* **Expose `sandboxd` gRPC as the only client API.** That is the right runtime interface and the wrong operator catalog. The current router also cannot proxy that gRPC. Tools that only need a terminal would have to speak `ProcessService`.
* **Treat `kubectl exec` as the standard.** It is a workable adapter underneath SAP. It is not a discovery record, and it is not a stable frame protocol a browser or a non-Kubernetes client can implement.

## Security Considerations

Authorization runs before list and before attach. The protocol does not add a client-supplied upstream address. Attach cannot select argv. Session bytes are terminal contents and must not be written to ordinary logs by default. A companion service that watches Sandboxes needs the same RBAC discipline as the router, not a cluster-admin credential in the client.
