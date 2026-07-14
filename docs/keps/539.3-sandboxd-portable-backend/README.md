# KEP-539.3: sandboxd Portable Backend — Hybrid gRPC/REST Protocol

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Current State](#current-state)
  - [Existing Filesystem API](#existing-filesystem-api)
  - [Existing SDK Wire Format](#existing-sdk-wire-format)
- [Proposal](#proposal)
  - [Hybrid Protocol Design](#hybrid-protocol-design)
  - [Process Service — gRPC](#process-service--grpc)
  - [Filesystem Service — REST/OpenAPI](#filesystem-service--restopenapi)
  - [Runtime Endpoints](#runtime-endpoints)
- [Breaking Changes](#breaking-changes)
  - [Endpoint Surface Changes](#endpoint-surface-changes)
  - [Wire Format Changes](#wire-format-changes)
- [Migration Plan](#migration-plan)
  - [SDK Versioning Strategy](#sdk-versioning-strategy)
  - [Transition Period](#transition-period)
- [Security Considerations](#security-considerations)
- [Implementation Phases](#implementation-phases)
- [Alternatives Considered](#alternatives-considered)
<!-- /toc -->


## Summary

This proposal defines the hybrid gRPC/REST protocol for `sandboxd`, the portable backend daemon for agent-sandbox. It formalizes the decision to use gRPC for process management (streaming) and REST/OpenAPI for filesystem operations (stateless), documents all breaking changes that will occur when the SDK is updated to target `sandboxd`, and defines the SDK migration strategy.


## Motivation

The current sandbox runtime is a FastAPI Python server. It is not portable, not typed, and couples the agent SDK directly to a Python HTTP server implementation. `sandboxd` replaces it with a purpose-built Go daemon that exposes a stable, versioned protocol.

### Goals

- Replace the existing `python-runtime` filesystem API with a typed `sandboxd` REST API (`filesystem.yaml`).
- Replace ad-hoc process execution with a typed gRPC `ProcessService` (`process.proto`).
- Define a clear SDK migration path so existing clients are not silently broken.
- Serve both protocols from the same sidecar container over separate, explicit ports.

### Non-Goals

- Maintaining backwards compatibility with the existing `python-runtime` unversioned API.
- Implementing Jupyter or Admin services (deferred to follow-up specification PRs).
- Changing the Kubernetes controller, `Sandbox` CRD, or networking layer (`sandbox-router`).


## Current State

### Existing Filesystem API

The `python-runtime` exposes an unversioned HTTP API consumed by the Go and Python SDKs today:

| Method | Endpoint | Purpose |
|---|---|---|
| `POST` | `/upload` | Write a file (multipart, flat filename only) |
| `GET` | `/download/{path}` | Read a file |
| `GET` | `/list/{path}` | List directory contents |
| `GET` | `/exists/{path}` | Check if path exists |

Limitations:
- `POST /upload` only accepts flat filenames — no subdirectory or parent creation support.
- No `DELETE` operation.
- No permission bit management on write (`mode`).
- Unversioned (no `/v1/` prefix).
- No process management — commands run via a separate ad-hoc execution mechanism.

### Existing SDK Wire Format

The Go SDK `FileEntry` type currently on the wire:

```go
type FileEntry struct {
    Name    string   `json:"name"`
    Type    FileType `json:"type"`     // "file" | "directory"
    Size    int64    `json:"size"`
    ModTime float64  `json:"mod_time"` // Unix epoch, float64
}
```


## Proposal

### Hybrid Protocol Design

`sandboxd` serves two protocols from separate ports within the sidecar container:

```text
sandboxd (sidecar)
├── gRPC  :9090  →  ProcessService    (streaming process I/O)
└── HTTP  :8080  →  FilesystemService (stateless file operations & runtime probes)
```

Both ports bind to `localhost` within the pod network namespace and are never exposed outside the container without explicit proxying. The agent SDK discovers them via environment variables:

```bash
SANDBOXD_GRPC_ADDR=localhost:9090
SANDBOXD_REST_ADDR=localhost:8080
```

### Process Service — gRPC

Defined in `packages/sandboxd/spec/process/v1/process.proto`.

gRPC is used for process management because `Start` is a long-lived server-streaming RPC — `stdout` and `stderr` flow continuously from the server until the process exits. Client input is handled separately via the `WriteStdin` RPC. HTTP/1.1 cannot model this cleanly.

| RPC | Type | Purpose |
|---|---|---|
| `Start` | Server stream | Run a command, stream `stdout`/`stderr` in real time until `ExitEvent` |
| `Execute` | Unary | Run a command synchronously, return `stdout`/`stderr`/`exit_code` atomically on exit |
| `WriteStdin` | Unary | Send `stdin` bytes or `EOF` to a running process |
| `SendSignal` | Unary | Deliver a POSIX signal (`SIGINT`, `SIGTERM`, `SIGKILL`); errors returned via gRPC status |
| `ResizeTTY` | Unary | Resize the pseudo-terminal window (`cols`, `rows`) |

### Filesystem Service — REST/OpenAPI

Defined in `packages/sandboxd/spec/filesystem/v1/filesystem.yaml` (introduced in companion PR #1116).

REST is used for filesystem operations because every operation is a simple request/response with a file payload — standard HTTP semantics (`GET`, `PUT`, `DELETE`) map naturally, avoiding base64 protobuf serialization wrapper overhead on large binary transfers. Any standard HTTP client works without generated stubs.

| Method | Endpoint | Purpose |
|---|---|---|
| `GET` | `/v1/files/{path}` | Read file (`application/octet-stream`) or list directory (`DirectoryListing` JSON) |
| `PUT` | `/v1/files/{path}` | Write file (`octet-stream` or `multipart/form-data`), creates parent dirs automatically |
| `DELETE` | `/v1/files/{path}` | Remove file or directory (supports `recursive=true` for `rm -rf` behavior) |
| `GET` | `/v1/health` | Liveness/readiness probe for Kubernetes (`200 OK` / `503 Service Unavailable`) |
| `GET` | `/v1/metadata` | Workload-scoped environment variables injected by the orchestrator (e.g. sandbox ID, workspace path) |

### Runtime Endpoints

- **`/v1/health`** is required for Kubernetes liveness and readiness probes. It returns `200 OK` (`{"status": "ok"}`) when ready to accept traffic and **`503 Service Unavailable`** when degraded or during shutdown. The orchestrator cannot use gRPC for standard probes — HTTP is mandatory here.
- **`/v1/metadata`** exposes workload-scoped environment variables injected by the orchestrator at pod creation time (e.g., sandbox ID, workspace path). It must never carry orchestrator credentials or Kubernetes API tokens — those must be kept outside the sandbox network namespace entirely.


## Breaking Changes

The `sandboxd` specification itself (#956, #1116) does not break existing clients today — existing SDKs continue to target the `python-runtime` API unchanged. The breaking change is deferred to the **SDK update (Part 3/3)**, when clients switch to point at `sandboxd` endpoints. At that point, the following changes will affect all SDK clients.

### Endpoint Surface Changes

| Existing | New (`sandboxd`) | Notes |
|---|---|---|
| `POST /upload` | `PUT /v1/files/{path}` | Method changed to `PUT` (idempotent); full relative paths supported; accepts both `octet-stream` and `multipart/form-data`; supports `mode` parameter validated by `^0[0-7]{3}$`. |
| `GET /download/{path}` | `GET /v1/files/{path}` | Renamed and versioned (`/v1/`). Returns `application/octet-stream`. |
| `GET /list/{path}` | `GET /v1/files/{path}` | Merged into single endpoint; dispatched by `application/json` content type. |
| `GET /exists/{path}` | `GET /v1/files/{path}` | No dedicated endpoint; `200 OK` means exists, `404 Not Found` means absent. |
| — | `DELETE /v1/files/{path}` | New — not available in existing `python-runtime` API. |

### Wire Format Changes

`FileEntry` changes between the existing SDK and the new `sandboxd` API:

| Field | Existing (`python-runtime`) | New (`sandboxd`) | Impact |
|---|---|---|---|
| `mod_time` | `float64` (Unix epoch) | **Removed** | **Breaking** — existing decoders will silently receive zero values. |
| `modified_at` | — | `string` (RFC 3339) | New field — ISO 8601 formatted timestamp. |
| `mode` | — | `string` (octal, e.g. `"0644"`) | New optional field — octal permission bits. |
| `name` | `string` | `string` | Unchanged. |
| `type` | `"file"` \| `"directory"` | `"file"` \| `"directory"` | Unchanged. Note: Symlinks are resolved by `SanitizePath` (`EvalSymlinks`) before listing, so `"symlink"` is never returned on the wire. |
| `size` | `int64` | `int64` | Unchanged. |

The SDK update must replace `ModTime float64` with `ModifiedAt time.Time` and update JSON unmarshalling accordingly.


## Migration Plan

### SDK Versioning Strategy

The existing Go and Python SDKs target the `python-runtime` API. `sandboxd` is a replacement, not an extension, making the migration a breaking SDK release.

Recommended approach:
1. **Determine current SDK version:** If the SDK is pre-v1.0, the breaking change can ship as a minor version bump with a prominent changelog entry. If it is already v1.x, it must ship as `v2.0.0`.
2. **Update SDK clients:** Update the filesystem client to target `sandboxd` REST endpoints (`/v1/files/...`), update the `FileEntry` struct/wire format, and implement a gRPC client for `ProcessService`.
3. **Gate on environment variables:** The SDK detects `SANDBOXD_REST_ADDR` and `SANDBOXD_GRPC_ADDR` to switch dynamically between the old `python-runtime` and new `sandboxd` endpoints, allowing a controlled rollout across different sandbox templates.

### Transition Period

| Phase | State |
|---|---|
| **Part 1/3 (Spec PRs #956, #1116)** | Protocol specifications defined (`process.proto`, `filesystem.yaml`); daemon not yet deployed; zero SDK impact. |
| **Part 2/3 (Runtime Daemon PR)** | `sandboxd` binary implemented (gRPC + HTTP servers, path sanitizer, process manager), shipped inside sidecar container image. |
| **Part 3/3 (SDK Migration PR)** | SDK updated to `sandboxd` endpoints; `python-runtime` officially deprecated. |
| **After Cutoff Date** | `python-runtime` container image removed; `sandboxd` becomes the sole supported runtime. |

The cutoff date must be agreed with the team before Part 3/3 ships. Announce the deprecation in the Part 2/3 release notes so SDK consumers have adequate lead time.


## Security Considerations

- **Network Containment:** Both ports (`:8080`, `:9090`) bind strictly to `localhost` inside the pod network namespace. They are not reachable from outside the pod without an explicit port forward or proxy (`sandbox-router`).
- **`/v1/metadata` & Untrusted Code:** The sandbox executes untrusted agent code. Any process inside the sandbox can read `/v1/metadata` via local loopback (`localhost`). Therefore:
  - `/v1/metadata` must only expose workload-scoped values: sandbox ID, workspace path, resource limits, and operator-approved non-sensitive environment variables.
  - Orchestrator credentials, Kubernetes API tokens, and cloud provider IAM keys must **never** be placed in `/v1/metadata`. Those must be injected via Kubernetes Secret volumes or projected service account tokens and permission-gated outside the agent's reach.
- **Path Traversal Protection:** All file paths received on `/v1/files/{path}` are processed through `SanitizePath`, which invokes `filepath.EvalSymlinks` and checks that the canonical resolved path strictly resides under the sandbox root (`/workspace`). Any attempt to traverse outside the sandbox root (`../`) is rejected with `403 Forbidden`.


## Implementation Phases

| Phase | Scope | Status |
|---|---|---|
| **Part 1/3: Protocol Specifications** | `ProcessService` proto (#956) + `FilesystemService` OpenAPI spec (#1116), `buf` toolchain (`buf.gen.yaml`) | **In Review / Merged** |
| **Part 2/3: Runtime Daemon** | `sandboxd` Go binary implementation (gRPC + HTTP server handlers, `SanitizePath`, process supervisor) | **Next** |
| **Part 3/3: SDK Migration** | SDK client update (Go + Python), `FileEntry` wire migration, `python-runtime` deprecation notice | **Planned** |
| **Follow-up Specifications** | `JupyterService` (kernel session manager) and `AdminService` definitions | **Deferred** |


## Alternatives Considered

- **gRPC for Filesystem Too:** E2B's `envd` uses gRPC (`filesystem.proto`) for metadata operations (`mkdir`, `stat`, `delete`, `watch`) and REST only for raw binary file transfer (`read`/`write`). We evaluated this but selected OpenAPI/REST for the entire filesystem surface because:
  - All `sandboxd` filesystem operations (`Read`, `Write`, `List`, `Remove`) are stateless request/response calls.
  - REST eliminates the need to compile and distribute generated gRPC stubs for filesystem I/O in every SDK language.
  - The design meeting confirmed REST-only for the filesystem layer.
- **Shared Port for gRPC and REST:** HTTP/2 content-type negotiation (`application/grpc` vs standard `application/json` / `octet-stream`) can serve both gRPC and REST on a single port. We chose separate ports (`:9090` vs `:8080`) to simplify the server implementation and make firewall and proxy routing rules explicit.
