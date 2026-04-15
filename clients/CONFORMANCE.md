# Agent Sandbox API Conformance

This document defines the behavioral expectations, constraints, and standard telemetry practices for implementing and consuming the `AgentSandboxService` gRPC API. Both the Sandbox server (e.g., router/runtime) and clients (Go, Python) MUST adhere to these definitions to ensure interoperability.

## 1. Error Handling and gRPC Status Codes

Implementations must map operational failures to standard gRPC status codes:

- **`INVALID_ARGUMENT` (3)**: 
  - The requested file path contains invalid characters or directory traversal attempts (e.g., `../`).
  - The `command` string in a `RunCommandRequest` is empty.
- **`NOT_FOUND` (5)**: 
  - The target path for `ReadFile` or `ListFiles` does not exist.
- **`DEADLINE_EXCEEDED` (4)**: 
  - The command execution exceeds the `timeout_seconds` defined in the request.
  - A filesystem operation times out.
- **`RESOURCE_EXHAUSTED` (8)**: 
  - Payload sizes exceed maximum allowed limits.
  - The sandbox runs out of Memory, CPU, or Storage space during command execution.
- **`FAILED_PRECONDITION` (9)**: 
  - The Sandbox environment is not currently ready or active (e.g., the pod is in hibernation/suspended state).
- **`UNAVAILABLE` (14)**:
  - The Sandbox router or underlying pod is unreachable or currently restarting. Clients SHOULD retry operations with exponential backoff on this code.

## 2. Resource Constraints & Payload Limits

To maintain stability across network boundaries, operations are subject to the following hard limits. Clients must enforce these limits before dispatching requests, and Servers must reject requests exceeding them with `RESOURCE_EXHAUSTED`:

- **`RunCommandResponse`**: Standard output and error combined must not exceed **16 MB**.
- **`ReadFileResponse` / `WriteFileRequest`**: File payloads are capped at **256 MB**.
- **`ListFilesResponse` / `FileExistsResponse`**: Metadata responses are capped at **8 MB**.

*Note: If operations require moving larger files, implementations should consider out-of-band artifact storage mechanisms (e.g., mounting PVCs or uploading to object storage) rather than relying directly on the gRPC interface.*

## 3. Path Handling Constraints

For security and consistency, the following filesystem rules apply:

- **Directory Separators**: Sandbox environments are assumed to be POSIX-compliant (Linux-based). Paths must use forward slashes (`/`).
- **Isolation**: Operations must not escape their designated workspace. Depending on the runtime strictness, paths navigating above the workspace root (e.g., `../etc/passwd`) MUST be rejected with `INVALID_ARGUMENT`.
- **Plain Filename Enforcement**: Where APIs restrict actions to plain filenames (e.g., specific `WriteFile` implementations), passing nested paths like `dir/script.py` must result in an `INVALID_ARGUMENT` error.

## 4. Telemetry and Context Propagation

The Sandbox ecosystem uses **OpenTelemetry (OTel)** for distributed tracing. 

- **Clients** MUST inject W3C Trace Context headers (`traceparent` and `tracestate`) into the gRPC metadata for every RPC call.
- **Servers/Routers** MUST extract this context and link internal execution spans to the parent span.
- **Identity Metadata**: Clients MUST inject the following metadata keys on every request to ensure traffic is correctly routed through Kubernetes Gateways or SPDY tunnels:
  - `x-sandbox-id`: The target sandbox identifier.
  - `x-sandbox-namespace`: The target Kubernetes namespace.
  - `x-sandbox-port`: The target container port (default: 8888).

## 5. Idempotency & Retry Behaviors

- **Filesystem Operations**: `ReadFile`, `WriteFile`, `ListFiles`, and `FileExists` are considered idempotent. Clients MAY safely automatically retry these operations (up to a configured maximum, e.g., 6 times) on transient errors like `UNAVAILABLE` or HTTP `502/503/504` equivalents.
- **Command Execution**: `RunCommand` is **NOT** strictly idempotent. Clients MUST NOT automatically retry `RunCommand` requests unless explicitly opted-in by the user (e.g., providing a `WithMaxAttempts` option).
