# KEP-539.2: Standardizing Sandbox Runtime Interfaces

**Authors:** @barney-s, @Oneimu

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Why Standardize Runtime Interfaces?](#why-standardize-runtime-interfaces)
  - [The SDK Angle](#the-sdk-angle)
  - [Proposed Methods and Capabilities](#proposed-methods-and-capabilities)
    - [Core Execution](#core-execution)
    - [Filesystem Operations](#filesystem-operations)
    - [Stateful Code Interpretation (Jupyter)](#stateful-code-interpretation-jupyter)
  - [Choosing Between REST and gRPC](#choosing-between-rest-and-grpc)
    - [Option 1: OpenAPI / REST](#option-1-openapi--rest)
    - [Option 2: gRPC / Protobuf](#option-2-grpc--protobuf)
  - [Should We Support Both?](#should-we-support-both)
  - [Concrete Implementation: <code>sandboxd</code> Hybrid gRPC/REST Architecture](#concrete-implementation-sandboxd-hybrid-grpcrest-architecture)
    - [Process Service (<code>:9090</code> gRPC)](#process-service-9090-grpc)
    - [Filesystem Service (<code>:8080</code> REST/OpenAPI)](#filesystem-service-8080-restopenapi)
    - [Runtime Probes &amp; Metadata](#runtime-probes--metadata)
    - [Breaking Changes vs. Existing <code>python-runtime</code>](#breaking-changes-vs-existing-python-runtime)
      - [Endpoint Surface Changes](#endpoint-surface-changes)
      - [Wire Format Changes (<code>FileEntry</code>)](#wire-format-changes-fileentry)
    - [SDK Migration Plan](#sdk-migration-plan)
    - [Security Considerations](#security-considerations)
- [Conformance](#conformance)
  - [1. Protocol Adherence](#1-protocol-adherence)
  - [2. Conformance Levels](#2-conformance-levels)
  - [3. Automated Conformance Testing](#3-automated-conformance-testing)
- [High-Level Design](#high-level-design)
- [Scalability](#scalability)
- [Extensions](#extensions)
  - [Reusable Sandboxes (Setup &amp; Cleanup)](#reusable-sandboxes-setup--cleanup)
    - [Implementation Considerations for Reusability](#implementation-considerations-for-reusability)
  - [Process Exit Event Retention and <code>Wait</code> RPC](#process-exit-event-retention-and-wait-rpc)
    - [Proposed API Additions](#proposed-api-additions)
    - [Wait Semantics](#wait-semantics)
    - [Implementation Considerations for Exit Retention](#implementation-considerations-for-exit-retention)
    - [Non-Goals of This Extension](#non-goals-of-this-extension)
  - [Detached Processes](#detached-processes)
    - [Proposed API Additions for Detached Processes](#proposed-api-additions-for-detached-processes)
    - [Detached Process Semantics](#detached-process-semantics)
    - [Implementation Considerations for Detached Processes](#implementation-considerations-for-detached-processes)
    - [Non-Goals for Detached Processes](#non-goals-for-detached-processes)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP proposes the standardization of the interaction interface between AI agent client SDKs and the isolated sandbox runtimes managed by `agent-sandbox`. We explore two primary paradigms for this interface: a RESTful API defined by OpenAPI (inspired by Alibaba's OpenSandbox) and a gRPC-based protocol defined by Protobuf (inspired by E2B's `envd`). We also propose a standard set of methods to support, including first-class support for Jupyter kernels.

## Motivation

Currently, `agent-sandbox` provides a flexible Kubernetes controller for managing sandbox lifecycles (Pods, PVCs, Warm Pools), but the interface *inside* the sandbox for executing commands and managing files is not standardized. Our current examples use a simple FastAPI HTTP server, while industry alternatives like E2B use highly optimized gRPC daemons (`envd`).

As the project evolves, standardizing this interface is critical for:
1. **SDK Compatibility:** Allowing a single SDK to talk to different sandbox runtimes seamlessly.
2. **Vendor Neutrality:** Ensuring users are not locked into a specific runtime implementation.
3. **Feature Completeness:** Defining a baseline of capabilities (like file watching and Jupyter integration) that all compliant runtimes should support.

### Goals
- Define a standard set of methods for sandbox interaction.
- Compare REST/OpenAPI and gRPC/Protobuf paradigms for this interface.
- Propose a path forward for standardization in `agent-sandbox`.

### Non-Goals
- Mandating a specific isolation technology (e.g., gVisor vs Firecracker).
- Implementing the full standardization in this KEP.

## Proposal

### Why Standardize Runtime Interfaces?

Standardizing the runtime interface decouples the AI agent's logic from the infrastructure. An agent should be able to say "run this code" or "read this file" without caring whether the sandbox is a local Docker container, a secure gVisor pod on GKE, or a Firecracker microVM. Standardization enables:
- **Interoperability:** Different vendors can build compliant runtimes.
- **Rich Ecosystem:** Community-contributed extensions can rely on a stable API contract.

### The SDK Angle

The SDK is the primary interface for agent developers. A standardized runtime interface allows us to build robust, feature-rich SDKs in multiple languages (Python, Go, JS) that remain consistent. It also allows us to mimic popular industry APIs (like E2B) to ease migration, while maintaining a distinct, open-source backend.

### Proposed Methods and Capabilities

We propose supporting the following core capabilities, divided into functional areas:

#### Core Execution
- `Run(command string) -> Result`: Execute a shell command.
- `Kill(sessionId string)`: Terminate a running process.
- `StreamOutput(sessionId string) -> Stream`: Real-time streaming of `stdout`/`stderr`.
- `StreamInput(sessionId string, data bytes)`: Sending input to a running process (`stdin`).

#### Filesystem Operations
- `Write(path string, content bytes)`: Upload/write a file.
- `Read(path string) -> bytes`: Download/read a file.
- `List(path string) -> []FileEntry`: List directory contents with metadata.
- `Stat(path string) -> FileMetadata`: Get file details.
- `Watch(path string) -> Stream[WatchEvent]`: Subscribe to real-time filesystem changes (crucial for workspace syncing).

#### Stateful Code Interpretation (Jupyter)
Unlike raw process execution, AI agents frequently need to execute Python snippets in a shared, stateful context. We propose first-class support for:
- `CreateContext() -> ContextId`: Initialize a stateful execution session (e.g., attaching to a Jupyter kernel).
- `ExecuteCode(ContextId, code string) -> ExecutionResult`: Run code within that context and return rich outputs (text, images, charts).




### Choosing Between REST and gRPC

#### Option 1: OpenAPI / REST
Inspired by Alibaba's OpenSandbox, this approach uses standard HTTP methods and JSON payloads.

* **Pros:**
    * **Simplicity:** Easy to understand, debug, and inspect. Requires no special gRPC toolchains, making it accessible to any developer familiar with HTTP.
    * **Universal Multi-Language Support:** Easy to generate clients in any programming language from the OpenAPI spec.
    * **Ecosystem Fit:** Fits naturally into standard web hooks and tool-calling patterns of LLMs and popular agent frameworks.
    * **Eliminating "Environment Drift":** A strict OpenAPI contract ensures that the runtime behavior is consistent across environments.
    * **Enterprise Security and Observability:** HTTP traffic is easy to inspect, log, and secure using standard enterprise tools (JWT, OIDC, standard reverse proxies).
* **Cons:**
    * **Performance:** Higher overhead for rapid, small payload executions.
    * **Streaming:** SSE or WebSockets are required for streaming, which can be less robust than native gRPC streams.

#### Option 2: gRPC / Protobuf
Inspired by E2B's `envd`, this approach uses a binary protocol over HTTP/2.

* **Pros:**
    * **High Performance:** Binary serialization and multiplexing offer ultra-low latency.
    * **Native Streaming:** Excellent support for bi-directional streaming (ideal for interactive terminals and file watching).
* **Cons:**
    * **Complexity:** Requires Protobuf toolchains and generated code, raising the barrier to entry for simple integrations.
    * **Inspectability:** Binary traffic is harder to inspect and debug without specialized tools.


### Should We Support Both?

**We propose a hybrid or pluggable approach rather than strictly choosing one.**

1. **Protocol-First Definition:** We should define the *capabilities* and *data models* abstractly.
2. **Pluggable Transports:** 
   * We can provide an **OpenAPI/REST** interface as the default, high-compatibility entry point for most users and languages.
   * We can support a **gRPC** interface for performance-critical workloads or when advanced streaming (like `Watch` or interactive PTY) is required.
3. **Sidecar Model:** By using a sidecar model in our Kubernetes Pods, users could choose to inject an `execd` (REST) sidecar or an `envd` (gRPC) sidecar depending on their SDK and performance needs, while keeping the underlying `agent-sandbox` CRD management identical.

### Concrete Implementation: `sandboxd` Hybrid gRPC/REST Architecture

To realize the hybrid model described above without forcing users to choose between two separate binaries (`execd` vs `envd`), we propose a unified portable daemon called **`sandboxd`** that serves both protocols from explicit, dedicated ports in whichever container hosts the daemon:

```text
sandboxd (runtime daemon)
├── gRPC  :9090  →  ProcessService    (streaming process I/O)
└── HTTP  :8080  →  FilesystemService (stateless file operations & runtime probes)
```

`sandboxd` can be the Sandbox's dedicated runtime container or can be injected into an existing application image. A separate sidecar is also possible, but commands run inside that sidecar and cannot use binaries from neighboring containers; containers share the pod network and mounted volumes, not their root filesystems.

Both ports bind to `0.0.0.0` by default, so clients with pod-network access can reach them through the Pod IP or a Service. Deployments must provide pod isolation and NetworkPolicy; use `--listen-host=127.0.0.1` when loopback-only access is required. The current `sandbox-router` accepts HTTP/2 client connections, but disables HTTP/2 on its upstream connections and therefore cannot carry the gRPC `ProcessService`, so it is not a complete transport for `sandboxd`.

Workload-local or custom clients may use environment variables such as the following to configure their own endpoint discovery:

```bash
SANDBOXD_GRPC_ADDR=localhost:9090
SANDBOXD_REST_ADDR=localhost:8080
```

These variables are a convention for application code, not an automatic SDK switch. The supported SDKs select `sandboxd` and its connectivity explicitly through their client configuration.

#### Process Service (`:9090` gRPC)
Defined in `packages/sandboxd/spec/process/v1/process.proto`.

gRPC is selected for process management because `Start` is a long-lived server-streaming RPC — `stdout` and `stderr` flow continuously from the server until the process exits. Client input is handled separately via unary `WriteStdin`. HTTP/1.1 cannot model this cleanly.

| RPC | Type | Purpose |
|---|---|---|
| `Start` | Server stream | Run a command, stream `stdout`/`stderr` in real time until `ExitEvent` |
| `Execute` | Unary | Run a command synchronously, return `stdout`/`stderr`/`exit_code` atomically on exit |
| `WriteStdin` | Unary | Send `stdin` bytes or `EOF` to a running process |
| `SendSignal` | Unary | Deliver a POSIX signal (`SIGINT`, `SIGTERM`, `SIGKILL`); errors returned via gRPC status |
| `ResizeTTY` | Unary | Resize the pseudo-terminal window (`cols`, `rows`) |

#### Filesystem Service (`:8080` REST/OpenAPI)
Defined in `packages/sandboxd/spec/filesystem/v1/filesystem.yaml`.

REST is selected for filesystem operations because every operation is a simple request/response with a file payload — standard HTTP semantics (`GET`, `PUT`, `DELETE`) map naturally, avoiding base64 protobuf serialization wrapper overhead on large binary transfers. Any standard HTTP client works without generated stubs.

| Method | Endpoint | Purpose |
|---|---|---|
| `GET` | `/v1/files/{path}` | Returns `application/octet-stream` for files or `application/json` (`DirectoryListing`) for directories; response type is determined server-side by the path target |
| `HEAD` | `/v1/files/{path}` | Check existence and retrieve metadata without transferring the body; `200` if exists, `404` if absent |
| `PUT` | `/v1/files/{path}` | Write file (`octet-stream` or `multipart/form-data`), creates parent dirs automatically |
| `DELETE` | `/v1/files/{path}` | Remove file or directory (supports `recursive=true` for `rm -rf` behavior) |
| `GET` | `/v1/health` | Liveness/readiness probe for Kubernetes (`200 OK` / `503 Service Unavailable`) |
| `GET` | `/v1/metadata` | Workload-scoped environment variables injected by the orchestrator (e.g. sandbox ID, workspace path) |

#### Runtime Probes & Metadata
- **`/v1/health`** is required for Kubernetes liveness and readiness probes. It returns `200 OK` (`{"status": "ok"}`) when ready to accept traffic and **`503 Service Unavailable`** when degraded or during shutdown. HTTP is chosen over gRPC probes for broader compatibility — while Kubernetes has supported native gRPC probes since v1.24, HTTP probes work across all supported Kubernetes versions and require no additional probe configuration.
- **`/v1/metadata`** exposes workload-scoped environment variables injected by the orchestrator at pod creation time (e.g., sandbox ID, workspace path). It must never carry orchestrator credentials or Kubernetes API tokens — those must be kept outside the sandbox network namespace entirely.

#### Breaking Changes vs. Existing `python-runtime`
The `sandboxd` wire protocol is not compatible with the unversioned `python-runtime` API (`POST /upload`, `GET /download/...`, `GET /list/...`). The supported SDKs avoid silently changing existing clients by keeping the legacy runtime available and selecting `sandboxd` explicitly. Applications that switch runtimes must also select a `sandboxd`-compatible transport and account for the endpoint and wire-format differences below.

##### Endpoint Surface Changes
| Existing (`python-runtime`) | New (`sandboxd`) | Notes |
|---|---|---|
| `POST /upload` | `PUT /v1/files/{path}` | Method changed to `PUT` (idempotent); full relative paths supported; accepts both `octet-stream` and `multipart/form-data`; supports `mode` parameter validated by `^0[0-7]{3}$`. |
| `GET /download/{path}` | `GET /v1/files/{path}` | Renamed and versioned (`/v1/`). Returns `application/octet-stream`. |
| `GET /list/{path}` | `GET /v1/files/{path}` | Merged into single endpoint; server returns `application/json` for directories and `application/octet-stream` for files. |
| `GET /exists/{path}` | `HEAD /v1/files/{path}` | No dedicated endpoint; `200` means exists, `404` means absent. `HEAD` avoids transferring the file body. |
| — | `DELETE /v1/files/{path}` | New — not available in existing `python-runtime` API. |

##### Wire Format Changes (`FileEntry`)
| Field | Existing (`python-runtime`) | New (`sandboxd`) | Impact |
|---|---|---|---|
| `mod_time` | `float64` (Unix epoch) | **Removed** | **Breaking** — existing decoders will silently receive zero values. |
| `modified_at` | — | `string` (RFC 3339) | New field — ISO 8601 formatted timestamp. |
| `mode` | — | `string` (octal, e.g. `"0644"`) | New optional field — octal permission bits. |
| `name` | `string` | `string` | Unchanged. |
| `type` | `"file"` \| `"directory"` | `"file"` \| `"directory"` | Unchanged. Note: Symlinks are resolved by `SanitizePath` (`EvalSymlinks`) before listing, so `"symlink"` is never returned on the wire. |
| `size` | `int64` | `int64` | Unchanged. |

#### SDK Migration Plan
1. **Explicit Runtime Selection:** The Go SDK selects `sandboxd` with `Options.Runtime = RuntimeSandboxd`; the synchronous Python SDK selects it with `SandboxdPodTunnelConnectionConfig`. The legacy `python-runtime` remains available and is still the default when `sandboxd` is not selected.
2. **Explicit Connectivity Selection:** The Go SDK and synchronous Python SDK can port-forward directly to the sandbox Pod. The Go SDK additionally supports direct in-cluster Pod IP and headless Service connectivity. These in-cluster modes require an ingress policy that admits the calling workload; the default managed NetworkPolicy allows ingress only from `sandbox-router`. Gateway connectivity through the current `sandbox-router` is not supported for `sandboxd` because the router does not proxy the gRPC surface.
3. **Application Endpoint Convention:** `SANDBOXD_REST_ADDR` and `SANDBOXD_GRPC_ADDR` may be injected for workload-local or custom clients, but the supported SDKs do not inspect these variables or automatically fall back between runtimes.

#### Security Considerations
- **Network Containment:** Both ports (`:8080`, `:9090`) bind to `0.0.0.0` by default and are reachable on the pod network. Deployments must restrict access with pod isolation and NetworkPolicy, or set `--listen-host=127.0.0.1` for loopback-only access. Never expose either port directly to an untrusted public network.
- **Transport Authentication:** `sandboxd` does not authenticate clients or terminate TLS. Pod port-forward access is authorized by the Kubernetes API server; direct pod-network access must be protected by NetworkPolicy, a service mesh or mTLS proxy, or another trusted transport boundary.
- **`/v1/metadata` & Untrusted Code:** The sandbox executes untrusted agent code which can query `/v1/metadata` via local loopback. Therefore, `/v1/metadata` must only expose non-sensitive workload configuration (sandbox ID, workspace path, resource limits). Orchestrator credentials, Kubernetes API tokens, and cloud provider keys must **never** be placed in `/v1/metadata`.
- **Path Traversal Protection:** All file paths received on `/v1/files/{path}` are processed through `SanitizePath`. For existing paths (reads, deletes, lists), `filepath.EvalSymlinks` resolves symlinks and verifies the canonical path resides under the sandbox root (`/workspace`). For new files (writes), `filepath.Clean` is applied lexically to the path and `filepath.EvalSymlinks` is applied to the parent directory to verify it does not escape the sandbox root. Traversal attempts (`../`) are rejected with `403 Forbidden`.

## Conformance

To ensure ecosystem interoperability, we propose a formal conformance definition for third-party runtimes. A runtime is considered "Agent Sandbox Compliant" if it meets the following criteria:

### 1. Protocol Adherence
- The runtime MUST implement all mandatory endpoints/methods defined in the official `agent-sandbox` OpenAPI or Protobuf specifications.
- Error codes and response formats MUST strictly match the spec.

### 2. Conformance Levels
To allow for lightweight or specialized runtimes, we propose tiered conformance:
- **Core Conformance:** Must support all methods in **Core Execution** and **Filesystem Operations** (except `Watch`). This is the minimum requirement.
- **Full Conformance:** Must support all Core methods PLUS **Jupyter Support** and filesystem **Watch** capabilities.

### 3. Automated Conformance Testing
- The project will provide a **Conformance Test Suite** (likely in Go or Python).
- This suite will run a battery of tests against a target sandbox endpoint to verify correct behavior of file creation, command execution, session management, and error handling.
- Vendors implementing custom runtimes can run this suite to validate their implementation.

## High-Level Design

The architecture will involve:
1. **Spec Repository:** Defining the OpenAPI YAMLs and Protobuf `.proto` files.
2. **Reference Runtimes:** Providing container images with the daemons (`execd` or `envd` clones) pre-installed.
3. **SDK Updates:** Updating the Python and Go SDKs to support connecting to these standardized endpoints.

## Scalability

- **REST:** May introduce overhead at high frequency. Connection pooling and keep-alive will be critical.
- **gRPC:** Highly scalable for streaming, but requires managing persistent HTTP/2 connections which can be complex behind load balancers.

## Extensions

### Reusable Sandboxes (Setup & Cleanup)

For use cases where sandboxes are long-lived but need to be reset or reconfigured for different tasks dynamically (similar to tools like `envbuilder`), we propose supporting methods to re-initialize the environment without destroying the underlying Kubernetes Pod:
- `Setup(rootfs string | image string)`: Initializes or resets the filesystem using a specified rootfs tarball or container image. This allows applying a specific environment configuration to a warm, running sandbox quickly.
- `Clean()`: Wipes the sandbox filesystem clean (or reverts it to a base state), removing any state left by previous executions, making it ready for a new `Setup` or execution run.

#### Implementation Considerations for Reusability
To ensure a truly clean slate and prevent state leakage between tasks, the `Clean()` operation must handle more than just the filesystem:
- **Efficient Filesystem Reset:** Consider using technologies like **OverlayFS** for near-instantaneous cleanup by simply wiping the writable upper layer and preserving the read-only base layer.
- **Process Cleanup:** All background processes spawned by the agent must be terminated (e.g., via cgroups).
- **Memory & Kernel State:** Language kernels (like Jupyter) must be restarted to clear in-memory variables and imported modules.
- **Network State:** Ensuring all ports bound by the agent are released and listening sockets are closed.
- **Environment Variables:** Resetting the environment block to the default "golden" state.

### Process Exit Event Retention and `Wait` RPC

sandboxd currently ties process exit information to the lifetime of the `Start` stream. The `ExitEvent` is sent on the same server-streaming connection that carries the process's output. When the process exits, `Start` records the exit code on the `ManagedProcess` — including on the disconnect path, where a SIGKILLed process is recorded as `137` (`128 + signal number`, as a shell reports it) — but the handler's deferred `ProcessRegistry.Remove` runs immediately afterwards, so no later RPC can read it: any RPC referencing that process ID fails with `NOT_FOUND`. If the stream breaks while the process is still running, sandboxd additionally kills the process tree (existing `killProcessGroupOnContextDone` behavior, unchanged by this extension), so the recorded outcome is sandboxd's own kill rather than the process's natural exit.

This extension retains each process's exit metadata (exit code, completion time) for a bounded TTL after exit and adds a blocking `Wait` RPC so clients can retrieve it. Without detached execution, it serves three client scenarios today:

- **Waiting from a separate call path.** A client — or a second concurrent consumer — can block on a process's outcome without holding or draining the output stream. A `Wait` timeout or cancellation ends only the RPC and never affects the process.
- **The natural-exit race.** When a process is reaped on its own around the moment the stream is lost, the kill watcher skips the already-reaped process and the *real* exit code is recorded — but is unreachable today. Retention makes it replayable, so an SDK can distinguish "exited with code N" from "killed by sandboxd" without re-running a non-idempotent command.
- **A definitive outcome after a disconnect.** A client that reconnects after a mid-run disconnect gets `NOT_FOUND` today and can only guess or re-run. With retention it gets a definitive `137` with a completion time.

What retention alone cannot solve is recovering the natural outcome of a *still-running* process across a mid-run disconnect: under kill-on-disconnect, that outcome is always `137`. Recovering real results in that case requires processes to survive disconnection — detached execution, proposed in #2061 and specified in [Detached Processes](#detached-processes). This extension is the retrieval half of that story: `Wait` is fully useful on its own, and `ExitEventStore` is designed now so that once processes survive disconnection, replaying their natural exit codes needs no further API change.

E2B's `envd` retains terminal process events for a bounded window (30 seconds), and a reconnecting client reads them through `Connect`. The precedent validates the retention *mechanism* — a small TTL-bounded store of exit metadata — but envd's reconnect guarantee rests on processes surviving cancellation, which sandboxd does not offer today. This extension adopts the mechanism, not the guarantee.

#### Proposed API Additions

Changes to `packages/sandboxd/spec/process/v1/process.proto` (all additive; no existing message or behavior changes):

```protobuf
service ProcessService {
  // Existing: Start, Execute, WriteStdin, SendSignal, ResizeTTY
  rpc Wait(WaitRequest) returns (WaitResponse);   // new
}

message WaitRequest {
  int32 process_id = 1;
  // Optional per-request timeout for the wait, supplied by the client and
  // enforced by the server: when it elapses, Wait returns DEADLINE_EXCEEDED.
  // When unset or 0, the server imposes no timeout of its own and relies on
  // the gRPC context deadline or cancellation. Servers reject negative
  // values, and values too large to convert to a duration, with
  // INVALID_ARGUMENT.
  optional int64 timeout_ms = 2;
}

message WaitResponse {
  int32 exit_code = 1;
  // Exit time as an RFC 3339 string, matching the modified_at convention
  // in the filesystem spec.
  string completed_at = 2;
}
```

New sandboxd daemon flags:

| Flag | Default | Behavior |
|---|---|---|
| `--exit-event-ttl` | `30s` | How long exit records remain queryable after exit. `0` disables retention; negative values are rejected at startup. |
| `--exit-event-max-entries` | `1024` | Maximum retained records. When full, the store evicts the oldest record by completion time. Values below `1` are rejected at startup; retention is disabled solely through `--exit-event-ttl=0`, keeping a single disable switch. |

#### Wait Semantics

| Process state | `Wait` result |
|---|---|
| Running, exits before timeout | `OK {exit_code, completed_at}` |
| Running, `timeout_ms` elapses | `DEADLINE_EXCEEDED`; the process is unaffected |
| Running, caller context cancelled | `CANCELLED`; the process is unaffected |
| `timeout_ms` negative, or too large to convert to a duration | `INVALID_ARGUMENT`; the process is unaffected |
| Exited, within TTL | `OK {exit_code, completed_at}` immediately |
| Unknown process ID, record expired, or record evicted at the entry cap | `NOT_FOUND` |
| Retention disabled (`--exit-event-ttl=0`) | Blocking waits on running processes still work; a `Wait` arriving after exit returns `NOT_FOUND` |

Key guarantees:

- **`Wait` never kills a process.** It is orthogonal to the stream-disconnect kill path; a timeout or cancellation ends only the RPC.
- **Concurrent waiters are supported.** Multiple `Wait` calls for the same process ID block on the process's existing `Done` channel. Waiters that remain attached until the process exits all receive the same exit outcome; an individual waiter can instead return `DEADLINE_EXCEEDED` or `CANCELLED` per its own timeout or cancellation, as the semantics table specifies.
- **Record before remove.** When retention is enabled, `Start`'s teardown records the exit event before removing the process from the registry, so no `Wait` call can observe a window where the process appears in neither store. This ordering holds on every teardown path, including the `InitEvent` send-failure path, where the client never learns the process ID: recording there too keeps the invariant unconditional and the implementation free of special cases. With `--exit-event-ttl=0` nothing is recorded, and a `Wait` arriving after removal returns `NOT_FOUND` as the semantics table specifies.

#### Implementation Considerations for Exit Retention

- A new `ExitEventStore` type in `packages/sandboxd/pkg/processmanager` holds `map[int32]exitRecord` guarded by a mutex. Each record stores only `{exit_code, completed_at}` (~40 bytes); the store must not retain references to `ManagedProcess`, pipes, or PTY handles, so resource cleanup behavior is unchanged.
- One daemon-wide cleanup goroutine expires records on a ticker. The daemon owns this goroutine and stops it when its context is cancelled during shutdown. When retention is disabled (`--exit-event-ttl=0`) no ticker is started — Go's `time.NewTicker` panics on a non-positive duration. For positive TTLs the tick interval is roughly TTL/2, clamped to a minimum floor (for example, 1s) so very small TTLs do not cause excessive wakeups. The entry cap is enforced inside `Record` by evicting the oldest completion time. With the defaults, the store is bounded at ~40KB and returns to zero when idle.
- The `Wait` handler resolves the process ID in order: live registry hit → block on the existing `Done` channel, then read the exit code and completion timestamp from the `ManagedProcess`; registry miss but store hit → check the record's `completed_at` against the TTL and replay the outcome only if it has not expired (lazy expiration at lookup time — periodic cleanup alone could leave an expired record queryable until the next tick); both miss, or expired → `NOT_FOUND`.
- **Authoritative `completed_at`.** `ManagedProcess` today records only the exit code. The exit path extends that shared completion state to record the completion timestamp in the same critical section as the exit code, and both the live `Wait` path and the store's record read this single value. Implementations must not synthesize a timestamp after `Done` closes.
- **Virtual PID wrap-around:** `ProcessRegistry.NextPID` documents that IDs are unique among concurrently live processes. Retention grows the uniqueness domain to "live plus retained records". With a monotonically increasing counter and a 30s TTL a collision is practically impossible; defensively, `NextPID` must not hand out an ID that is still present in the retention store (it skips to the next value). Reserving retained IDs removes the whole class of stale-waiter confusion, including at the registry level, where a reused ID would otherwise surface a new process to callers holding the old one. `Record` correspondingly never overwrites a retained record. These guarantees are scoped to a single daemon instance; see the next bullet.
- **Daemon restarts invalidate process IDs.** The registry counter and the retention store are in-memory, so a sandboxd restart clears both and reissues IDs from the beginning. A client that carries a pre-restart ID into the new instance could match an unrelated new process. This exposure predates the extension — `SendSignal`, `WriteStdin`, and `ResizeTTY` share it today — and clients can detect restarts through the existing `/v1/health` `uptime_seconds` field, which drops back after a restart. Scoping process identity to a daemon generation across `ProcessService` is left to future work.
- `Execute` does not participate: it is a synchronous unary call that returns the exit code atomically and never registers a process.
- Testing: `ExitEventStore` unit tests (TTL expiry, cap eviction, disabled mode); one test per row of the semantics table, asserting that timeout/cancellation leave the process running; a record-before-remove race test; concurrent waiters; an integration test that drops the stream mid-run and asserts `Wait` replays exit code `137`, documenting the kill-path outcome rather than a survival guarantee; and a natural-exit race test where the process exits on its own before the stream is dropped and `Wait` must replay its real exit code.

#### Non-Goals of This Extension

- Changing the "stream disconnect kills the process tree" behavior. Detached execution (#2061, specified in [Detached Processes](#detached-processes)) owns that change; retention reaches its full reconnect value only after it lands.
- Retaining or replaying `stdout`/`stderr` output; only exit metadata is retained.
- Stream reattachment; [Detached Processes](#detached-processes) adds it as the `Attach` RPC, alongside detached execution (#2061).
- Persisting exit events across sandboxd restarts. In a Pod, child processes do not outlive the daemon.
- Daemon-generation-scoped process identity. Process IDs are valid only within a single sandboxd instance; making them safe to carry across daemon restarts is a `ProcessService`-wide concern left to future work.

Conformance impact is deferred: `Wait` becomes a candidate for the conformance suite only after the reference implementation lands, so this extension does not change the obligations of third-party runtimes today.

### Detached Processes

Long-lived sandboxes run dev servers, watchers, and builds that should keep running when the client that started them restarts or reconnects. #2061 asks for this. Today the end of a `Start` stream triggers `killProcessGroupOnContextDone`, which SIGKILLs the process group. #2061 describes the workaround clients use instead. They start the command under a `setsid` launcher, write its output and exit code to files on a volume, poll those files from a follower script, and count zombies as dead because sandboxd does not reap orphans (#1980).

This extension adds detached processes. A detached process is a process that a client starts with the new `StartRequest.detached` field set. sandboxd keeps it running after the `Start` stream ends, keeps its output in a bounded buffer, and keeps its exit record until a client calls `Release` or sandboxd evicts it. Clients find detached processes with `List`, read their output with `Attach`, signal them with the existing `SendSignal`, and free them with `Release`.

With detached execution, `Wait` and exit retention from [the previous section](#process-exit-event-retention-and-wait-rpc) return the process's own outcome after a disconnect. Under kill-on-disconnect, a process that is still running when the stream breaks always reports `137`, which is sandboxd's own kill. A detached process keeps running through the disconnect. When the client reconnects, `Wait` returns the exit code the process produced itself, and `Attach` returns the output it wrote in the meantime.

#### Proposed API Additions for Detached Processes

This extension changes `packages/sandboxd/spec/process/v1/process.proto` as follows. All changes are additive, and a `Start` without `detached` behaves as it does today.

```protobuf
service ProcessService {
  // Existing RPCs are unchanged. This extension assumes the Wait RPC from
  // the previous section.
  rpc List(ListRequest) returns (ListResponse);               // new
  rpc Attach(AttachRequest) returns (stream AttachResponse);  // new
  rpc Release(ReleaseRequest) returns (ReleaseResponse);      // new
}

message StartRequest {
  ProcessConfig config = 1;
  optional PTY pty = 2;
  // New. When true, Start sends InitEvent and ends the stream with OK. The
  // process keeps running and its output goes to buffers that Attach reads.
  bool detached = 3;
}

message InitEvent {
  int32 process_id = 1;
  // New. True when sandboxd started the process detached.
  bool detached = 2;
}

enum ProcessState {
  PROCESS_STATE_UNSPECIFIED = 0;
  PROCESS_STATE_RUNNING = 1;
  PROCESS_STATE_EXITED = 2;
}

// Offsets count bytes since the process started, so they stay valid after
// the buffer drops old output.
message OutputRange {
  int64 start_offset = 1; // Earliest byte still retained.
  int64 end_offset = 2;   // Total bytes written so far.
}

message ProcessInfo {
  int32 process_id = 1;
  repeated string command = 2; // ProcessConfig.command; env_vars are omitted.
  ProcessState state = 3;
  bool detached = 4;
  optional int32 exit_code = 5;     // Set when state is EXITED.
  optional string completed_at = 6; // RFC 3339; set when state is EXITED.
  OutputRange stdout = 7;           // Detached processes only.
  OutputRange stderr = 8;           // Detached processes only.
}

message ListRequest {}

message ListResponse {
  repeated ProcessInfo processes = 1;
}

message AttachRequest {
  int32 process_id = 1;
  // Offsets to replay from. 0 replays everything still retained. Unset
  // starts at the current end and streams only new output.
  optional int64 stdout_offset = 2;
  optional int64 stderr_offset = 3;
}

// First event on every Attach stream. Reports where replay starts for each stream.
message AttachStartEvent {
  int64 stdout_offset = 1;
  int64 stderr_offset = 2;
  // True when the requested offset was older than the earliest retained byte.
  bool stdout_truncated = 3;
  bool stderr_truncated = 4;
}

message OutputChunk {
  int64 offset = 1; // Offset of the first byte in data.
  bytes data = 2;
}

message AttachResponse {
  oneof event {
    AttachStartEvent start = 1;
    OutputChunk stdout = 2;
    OutputChunk stderr = 3;
    ExitEvent exit = 4; // Existing message; always the last event.
  }
}

message ReleaseRequest {
  int32 process_id = 1;
}

message ReleaseResponse {}
```

This extension names the reattach RPC `Attach`, the term #2061 uses. `Attach` is not wire-compatible with envd's `Connect`, so it does not reuse that name.

New sandboxd daemon flags:

| Flag | Default | Behavior |
|---|---|---|
| `--detached-max-processes` | `16` | Maximum detached processes sandboxd tracks, counting running ones and exited ones not yet released. A detached `Start` at the cap evicts the exited detached process with the oldest `completed_at`. If every tracked detached process is still running, `Start` returns `RESOURCE_EXHAUSTED`. `0` disables detached execution, and a detached `Start` returns `FAILED_PRECONDITION`. sandboxd rejects negative values at startup. |
| `--detached-output-buffer-bytes` | `1048576` (1 MiB) | Size of the ring buffer for each output stream of each detached process. sandboxd rejects values below the stream chunk size (`--stream-chunk-size`, 4096 bytes by default) at startup. |

These two flags also bound retained output for the whole daemon. The bound is `--detached-max-processes` times two streams times `--detached-output-buffer-bytes`, which is 32 MiB with the defaults. A separate total-bytes flag would only restate this product, so this extension does not add one. A sandboxd instance whose clients have gone away therefore holds at most 16 detached processes and 32 MiB of retained output.

#### Detached Process Semantics

| Call | Detached, running | Detached, exited | Non-detached, running |
|---|---|---|---|
| `Attach` | Replays from the requested offsets, follows new output, and ends with `ExitEvent` after exit | Replays retained output, then ends with `ExitEvent` | `FAILED_PRECONDITION` |
| `Wait` | Blocks until exit through the existing registry path | `OK {exit_code, completed_at}` immediately, until `Release` or eviction, even with `--exit-event-ttl=0` | Unchanged |
| `SendSignal` | Signals the process group, as today | `OK` without sending a signal, as `ManagedProcess.Signal` does today for a completed process | Unchanged |
| `WriteStdin` | Unchanged | `FAILED_PRECONDITION` (new check) | Unchanged |
| `ResizeTTY` | Unchanged | `FAILED_PRECONDITION`, as today for a completed process | Unchanged |
| `List` | `RUNNING`, with output ranges | `EXITED`, with `exit_code`, `completed_at`, and output ranges | `RUNNING`, without output ranges |
| `Release` | `FAILED_PRECONDITION`; the process keeps running | Frees the buffers and the exit record | `FAILED_PRECONDITION` |

Every call returns `NOT_FOUND` for an unknown or released process ID, as today. A non-detached process leaves `List` when it exits. After that, only `Wait` can find it, through `ExitEventStore`. `Execute` never registers a process, so `List` never shows the processes it runs.

| `Attach` offset | Result |
|---|---|
| Unset | Starts at the current `end_offset` and streams only new output |
| From `start_offset` to `end_offset` | Replays from the requested offset |
| Below `start_offset` | Replays from `start_offset`. `AttachStartEvent` reports that offset and sets the stream's `truncated` flag. |
| Above `end_offset` | `OUT_OF_RANGE`. No valid offset for this process is that large. |
| Negative | `INVALID_ARGUMENT` |

Key guarantees:

- **Ending a stream never affects a detached process.** Cancelling or losing a `Start` or `Attach` stream ends only that RPC.
- **Attachers never slow the process.** sandboxd drains the pipes into the buffers whether or not a client is attached. Today `Start` sends each chunk on the stream, so gRPC flow control can stall a process whose client reads slowly. An attacher that falls behind a detached process loses the oldest bytes instead. The jump in `OutputChunk.offset` shows the attacher where the gap is.
- **Multiple clients can attach at once.** Each `Attach` stream reads the same buffers with its own offsets, and each one ends with the same `ExitEvent`.
- **Output before exit.** `Attach` sends `ExitEvent` only after the readers have finished and the stream has sent every retained byte up to the final `end_offset`.
- **`Release` never kills a process.** To stop and free a detached process, a client calls `SendSignal`, then `Wait`, then `Release`. If an `Attach` stream is still replaying the process, `Release` or eviction ends that stream with `NOT_FOUND`.

#### Implementation Considerations for Detached Processes

- **Ownership.** For a detached process, a daemon-owned goroutine takes over `cmd.Wait`, the output readers, and the registry entry from the `Start` handler. The handler does not call `killProcessGroupOnContextDone` or defer `ProcessRegistry.Remove`, and it returns after sending `InitEvent`. If that send fails, sandboxd kills the process and tears it down like a non-detached process, because the caller never learned the ID.
- **Output buffers.** A new `OutputBuffer` type in `packages/sandboxd/pkg/processmanager` holds one stream's ring buffer with its `start_offset` and `end_offset`, and wakes waiting attachers on each append. It allocates memory as output arrives, so a process that writes nothing allocates nothing. Readers read in `--stream-chunk-size` chunks, as `Start` does. PTY output goes to the stdout buffer, and the stderr range stays empty. Offsets count bytes, so replay can start inside a UTF-8 character or a terminal escape sequence. #2117 bounds unary `Execute` output with `executionStream`, which keeps the prefix. A follower needs the newest output, so `OutputBuffer` keeps the tail.
- **Exit.** When the process exits, the owning goroutine applies the same `waitDelay` and `pipeDrainGrace` bounds as `Start`. It then records the exit code and `completed_at` on the `ManagedProcess` and closes `Done`. This is the same completion state the previous section defines. It closes the pipes or PTY. The registry entry then holds only the ID, command, buffers, and exit state. sandboxd does not capture output that a surviving grandchild writes after the drain bound.
- **Composition with `ExitEventStore`.** Exit records follow one of two retention rules. A non-detached process moves to `ExitEventStore` at teardown and expires after `--exit-event-ttl`. A detached process keeps its record on its `ProcessRegistry` entry until `Release` or eviction, and never enters `ExitEventStore`. `--exit-event-ttl` governs only non-detached processes, even when it is `0`. With retention disabled, `Wait` still returns a detached process's exit result until `Release` or eviction. `Wait` needs no new code path for detached processes. It finds them in the registry, and the closed `Done` channel makes it return at once. `Release` and eviction do not copy the record into `ExitEventStore`. The store's lazy expiry compares `completed_at` against `--exit-event-ttl`, so a record that sandboxd kept longer than the TTL would expire as soon as it arrived.
- **Process IDs.** `NextPID` must also skip IDs held by the registry, which extends the previous section's rule for `ExitEventStore`. Detached entries can stay in the registry for hours, much longer than a 30-second exit record. Without this rule, a counter that wraps around could hand out the ID of an entry that still exists.
- **Process groups and reaping.** A detached process stays a direct child of sandboxd. sandboxd keeps the process group setup it uses today: `Setpgid` for pipe processes and `Setsid` (set by `creack/pty`) for PTY processes. It does not double-fork. The owning goroutine's `cmd.Wait` reaps the process sandboxd started, so sandboxd records its exit code and the process never stays a zombie. The kernel reparents grandchildren that outlive their parent to PID 1. When sandboxd is PID 1, those grandchildren stay zombies after they exit (#1980). Detached execution makes #1980 more pressing, because it targets long-lived trees such as dev servers and shells that background work. This extension depends on #1980 for orphan reaping. That reaper must not call a broad `waitpid(-1)`, because that call can collect a child that sandboxd still waits on through `cmd.Wait`. It must skip the OS PIDs of those children, which include every registry entry's `ManagedProcess.Cmd.Process.Pid` and the children of in-flight `Execute` calls. If the reaper collected a tracked child first, `cmd.Wait` would fail with `ECHILD`. sandboxd would then record `-1`, which is what `Start` records today for any `Wait` error that is not an `exec.ExitError`. One option is for the reaper to find an exited child with `waitid(P_ALL, 0, &info, WEXITED|WNOWAIT)`, which leaves the child waitable for `cmd.Wait`, and to reap that PID only if sandboxd does not track it. The reaper would check and reap under a lock that sandboxd also holds from before `cmd.Start` until it records the new PID as tracked, so the reaper cannot collect a child that exits before then.
- **Daemon restarts.** Detached processes and their records do not survive a sandboxd restart. `ShutdownProcesses` sends SIGTERM and then SIGKILL to every registered process, detached ones included. The previous section's "Daemon restarts invalidate process IDs" bullet applies unchanged. After a restart, a pre-restart process ID can match an unrelated new process, so `Attach` can return that process's output and `List` and `Wait` can report it. Clients detect the restart through the `/v1/health` `uptime_seconds` field, as the previous section describes, and should drop their saved process IDs and offsets.
- **Compatibility.** A sandboxd without this extension ignores the unknown `detached` field, keeps the process tied to the stream, and keeps streaming output. It never sets `InitEvent.detached`. If an SDK requested detached execution and `InitEvent.detached` is `false`, the SDK must treat detached execution as unsupported and report an error. Older daemons return `UNIMPLEMENTED` for `List`, `Attach`, and `Release`.
- **Security and limits.** The bearer-token interceptors from `Server.GRPCServerOptions` apply to every RPC on the gRPC server, so the new RPCs need no new authorization code. sandboxd trusts every client that holds its bearer token equally. Any such client can see every process through `List` and can call `Attach`, `SendSignal`, or `Release` on any process, including one that another client started. Per-client process ownership is out of scope for this extension. `List` omits `env_vars`, which can carry credentials. Retained output can also contain secrets the process printed. It stays in sandboxd memory until `Release` or eviction frees it. `--detached-max-processes` limits detached processes per sandboxd instance.
- Testing: `OutputBuffer` unit tests (wrap-around, offsets after overwrite, growth up to the cap); one test per row of both semantics tables; an integration test that starts a detached process, cancels the `Start` stream, reattaches with saved offsets, and receives the remaining output and the process's own exit code from `Attach` and `Wait`; concurrent attachers receiving identical bytes; a slow attacher that sees an offset gap while the process keeps running; `Release` on a running process returning `FAILED_PRECONDITION` with the process still alive; eviction order and `RESOURCE_EXHAUSTED` at `--detached-max-processes`; flag validation at startup; `ShutdownProcesses` terminating detached processes; and, once #1980 lands, a test that a tracked detached process keeps its own exit code while the #1980 reaper runs.

#### Non-Goals for Detached Processes

- Changing the default. A `Start` without `detached` still kills the process tree when its stream ends.
- Output retention or `Attach` for non-detached processes. Their output still flows only through the `Start` stream.
- Persisting detached processes across sandboxd restarts, and daemon-generation-scoped process IDs. The previous section's non-goals cover both.
- Client-assigned names or tags. Clients map process IDs to their own names, and `List` returns each process's command.
- Stdin over `Attach`. Input still goes through `WriteStdin`.
- Filtering or paging in `List`.
- Per-client process ownership or isolation within one sandboxd instance.

This extension defers conformance in the same way. `List`, `Attach`, `Release`, and `StartRequest.detached` become candidates for the conformance suite only after the reference implementation lands.

## Alternatives

- **Status Quo:** Continue letting examples define their own ad-hoc HTTP servers. This leads to fragmentation and prevents building a robust, reusable client SDK.
- **Strictly gRPC:** Win on performance but lose on simplicity and ease of adoption.
- **Strictly REST:** Win on simplicity but limit advanced use cases requiring high-performance streaming.
