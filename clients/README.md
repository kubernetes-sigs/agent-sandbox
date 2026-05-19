# Agent Sandbox SDK Clients

## Overview

The `agent-sandbox` project provides two high-level, developer-friendly SDKs: a **Go client** and a **Python client**. These SDKs abstract away the underlying Kubernetes complexities and provide a simplified interface to programmatically manage Sandbox lifecycles, execute commands, and read/write files within secure sandbox environments. 

Both clients are designed for building AI agents, code interpreters, and secure workload runners, offering native idioms and seamless integration for their respective ecosystems.

### Python Client (`k8s-agent-sandbox`)
The Python client is distributed via PyPI. It features a rich, idiomatic Python experience including robust sync and async APIs, Pydantic data models for configuration validation, optional OpenTelemetry capabilities, and **advanced extensions for GKE Pod Snapshots**. The async client natively uses context managers (`async with` statements) for automatic resource cleanup.

### Go Client
The Go client is provided as part of the `sigs.k8s.io/agent-sandbox` Go module. It brings feature parity to the Go ecosystem for core sandbox functionality, catering to platform services and high-performance, concurrent agentic applications by fully supporting the standard Go `context.Context` for execution timeouts and cancellation.

---

## Feature Comparison Matrix

| Feature | Python Client | Go Client |
|---------|:---:|:---:|
| **Sandbox Lifecycle Management (Create, Terminate)** | ✔️ | ✔️ |
| **Command Execution (`Run`)** | ✔️ | ✔️ |
| **File Operations (`WriteFile`, `ReadFile`)** | ✔️ | ✔️ |
| **Connection: Developer Mode (Local Tunneling)** | ✔️ | ✔️ |
| **Connection: Production Mode (Gateway)** | ✔️ | ✔️ |
| **Connection: In-Cluster (Direct Pod IP/DNS)** | ✔️ | ✔️ |
| **Warmpool Integration Support** | ✔️ | ✔️ (Adoption only) |
| **GKE Pod Snapshots (Create, List, Delete)** | ✔️ | ❌ |
| **GKE Sandbox Suspend (Scale to 0 + Snapshot)** | ✔️ | ❌ |
| **GKE Sandbox Resume (Restore from Snapshot)** | ✔️ | ❌ |
| **Automatic Cleanup / Safe Teardown** | ✔️ (Async Context Managers / Opt-in `atexit`) | ✔️ (`defer` statements) |
| **Asynchronous Concurrency** | ✔️ (`async` / `await` APIs) | ✔️ (Native Goroutines) |
| **Timeouts & Task Cancellation** | ✔️ (`asyncio` / kwargs) | ✔️ (`context.Context`) |
| **Data Type Safety & Validation** | ✔️ (Runtime via Pydantic) | ✔️ (Compile-time via Static Types) |
| **Built-in OpenTelemetry Tracing** | ✔️ | ✔️ |

---

## Detailed Differences

### 1. Concurrency and Async Patterns
*   **Python SDK:** Explicitly divides its API into synchronous (`SandboxClient`) and asynchronous (`AsyncSandboxClient`) sibling classes. This allows developers to seamlessly drop the SDK into asynchronous orchestrators, FastAPI applications, or `aiohttp` routines using `async`/`await`. *Note: Local tunneling (Developer Mode) relies on blocking subprocesses and is exclusively supported by the synchronous client.*
*   **Go SDK:** Relies on Go's native concurrency model. While the API surface is synchronous and network I/O blocks the calling goroutine, it is designed to be highly concurrent and performant when spawned across multiple standard goroutines.

### 2. Timeouts and Task Cancellation
*   **Python SDK:** Timeouts and cancellations are generally handled through `asyncio` timeouts, task cancellation, or specific configuration parameters passed during connection setup.
*   **Go SDK:** Has native, deep integration with `context.Context`. Every network request, wait-for-ready loop, and command execution accepts a context parameter, granting fine-grained control over cancellation and timeouts across the entire lifecycle.

### 3. Resource Cleanup
*   **Python SDK:** 
    *   **Async Client:** Designed to be used as an async context manager (`async with AsyncSandboxClient(...) as client:`), guaranteeing immediate cleanup when the execution scope exits, even on exceptions.
    *   **Sync Client:** Automatic cleanup on program termination (via `atexit`) is **disabled by default**. It must be explicitly enabled by initializing the client with the `cleanup=True` parameter.
*   **Go SDK:** Idiomatically uses `defer` to ensure cleanup (e.g., `defer client.Close(ctx)`). 

### 4. Observability and Extensibility
*   **Python SDK:** Provides optional built-in OpenTelemetry tracing support. By installing the client with the tracing extra (`pip install "k8s-agent-sandbox[tracing]"`), developers can enable OpenTelemetry export (e.g., to Google Cloud Trace via an OTLP collector). See the GCP Tracing Guide for details on backend configuration.
*   **Go SDK:** Includes built-in OpenTelemetry tracing support. The client automatically starts spans for key lifecycle operations and provides helper APIs (like `sandbox.NewTracerProvider`) to easily configure an OTLP gRPC exporter for your observability backend.

### 5. Type Safety and Validation
*   **Python SDK:** Because Python is dynamically typed, the SDK relies heavily on `Pydantic` for configuring connection modes (e.g., `SandboxGatewayConnectionConfig`). This provides strong runtime type-checking and validation of user inputs.
*   **Go SDK:** Relies on Go's strict static typing and native structs. Most validation (like ensuring a port is an integer) is caught at compile-time by the Go compiler. Additional runtime constraints are handled via explicit validation methods rather than a heavy reflection-based library.

### 6. GKE Pod Snapshot Extension (State Preservation)
*   **Python SDK:** Ships with a dedicated `PodSnapshotSandboxClient` extension tailored for GKE clusters running gVisor. This allows for advanced agentic workflows where a sandbox can be "parked" between user prompts to save costs. 
    *   **Snapshots:** Trigger manual snapshots (`sandbox.snapshots.create()`) to preserve the filesystem and memory state.
    *   **Suspend:** Seamlessly scale a sandbox down to 0 replicas (`sandbox.suspend(snapshot_before_suspend=True)`), actively halting compute cost while keeping the state intact.
    *   **Resume:** Instantly scale back to 1 replica (`sandbox.resume()`), automatically restoring the environment from the latest snapshot.
*   **Go SDK:** Currently lacks a high-level abstraction for GKE Pod Snapshots. Developers working in Go who need state preservation must manually interact with the standard Kubernetes Go client to create `PodSnapshotManualTrigger` custom resources alongside their `agent-sandbox` client logic.
