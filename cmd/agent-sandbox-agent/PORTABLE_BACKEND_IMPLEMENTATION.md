# Portable Agent Sandbox Backend Daemon (`agent-sandbox-agent`)
## Technical Architecture & Implementation Detail

This document provides a comprehensive, deep-dive technical explanation of the **Portable Agent Sandbox Backend Daemon** (`agent-sandbox-agent`) implemented in Go. 

This daemon runs as a lightweight background process (or sidecar container) directly inside the sandbox workload container, exposing a modular, language-agnostic gRPC interface to enable high-performance command execution, file operations, stateful Python runtimes, and environment resets.

---

## 1. Architectural Philosophy: Data Plane vs. Control Plane

The `agent-sandbox` ecosystem is split into two strictly decoupled layers:

1.  **The Control Plane (`agent-sandbox-controller`)**: Runs at the cluster level. It coordinates the scheduling, warming, claiming, and lifecycle (creation, deletion, hibernation) of sandbox custom resources. It has *no knowledge* of the code execution itself.
2.  **The Data Plane (`agent-sandbox-agent`)**: Runs *inside* the sandbox container pod itself. It has *no knowledge* of Kubernetes, Custom Resources, or scheduling. It strictly exposes network-accessible endpoints to execute commands and manage files inside its own local Linux namespaces.

This strict decoupling delivers **unprecedented portability**. By standardizing the interaction API via gRPC, any client SDK (Python, Go, Java) can interact with any containerized sandbox (Python, Node.js, C++) in a unified way, without code modifications.

```
  [ Client SDK / LLM Agent ]
              │
              ▼ (gRPC over Port 50051)
┌────────────────────────────────────────────────────────────────────────┐
│                       Active GKE Sandbox Pod                           │
│                                                                        │
│  Container A: user-runtime            Container B: sandbox-daemon      │
│  ┌─────────────────────────┐          ┌─────────────────────────────┐  │
│  │  Python 3 / Jupyter     │◄─────────┤  agent-sandbox-agent        │  │
│  └───────────┬─────────────┘          │  (gRPC Server)              │  │
│              │                        └──────────────┬──────────────┘  │
│              │ (Shares filesystem namespaces)        │                 │
│              ▼                                       ▼                 │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                    Shared emptyDir Volume                        │  │
│  │                    MountPath: /workspace                         │  │
│  └──────────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Modular Service Architecture (gRPC API Contract)

Rather than exposing a monolithic API, the daemon splits its interface into four specialized domain services defined in **`api/proto/v1/agent_sandbox.proto`**:

| Service | Responsibility | Key RPC Methods |
| :--- | :--- | :--- |
| **`ProcessService`** | Spawns and controls local OS shell processes. | `Start` (streaming), `Execute` (sync), `SendSignal` (SIGINT/SIGKILL) |
| **`FilesystemService`** | Inspects and manipulates files on disk. | `WriteFile`, `ReadFile`, `ListFiles`, `StatFile`, `MakeDir`, `Remove` |
| **`JupyterService`** | Manages persistent, stateful Python runtimes. | `CreateSession`, `ExecuteCode` |
| **`AdminService`** | Resets the environment for sandbox reuse. | `Setup` (workspace extraction), `Clean` (PID wiping & format) |

---

## 3. Core Module Implementation Breakdown

The Go implementation resides under `cmd/agent-sandbox-agent/` and `internal/agent/`. Below is a detailed analysis of how each module functions at the system level.

### A. Server Entrypoint: `cmd/agent-sandbox-agent/main.go`
The launcher initializes the network listener and registers our modular service handlers with the gRPC engine:

*   **Network Binding**: Binds a standard TCP listener (`net.Listen`) on the configured port (default: `50051`).
*   **Service Registration**: Instantiates and binds `ProcessServer`, `FilesystemServer`, `JupyterServer`, and `AdminServer` to the active gRPC server engine.
*   **gRPC Reflection**: Registers the reflection service (`reflection.Register`). This publishes service metadata over the socket, allowing developers to inspect, explore, and test all endpoints instantly using generic command-line utilities (like `grpcurl`).
*   **Graceful Shutdown**: Intercepts OS termination signals (`SIGINT`, `SIGTERM`). It gracefully terminates the background Jupyter process pool and stops the gRPC server without interrupting active connections.

---

### B. Shell Command Execution: `internal/agent/process.go`
This module is responsible for running commands and streaming console output back in real-time.

*   **Synchronous Execution (`Execute`)**:
    *   Uses Go's `os/exec` package to invoke commands inside the container using `exec.CommandContext`.
    *   Injects a key-value map of isolated environment variables (`req.EnvVars`) merged with the daemon's local OS environment.
    *   Blocks execution until process completion, capturing the full stdout and stderr logs to return them along with the integer exit code.
*   **Real-Time Streaming (`Start`)**:
    *   Obtains direct I/O pipes (`StdoutPipe` and `StderrPipe`) from the child process.
    *   Launches concurrent Go routines to read raw bytes from these pipes into a `4096-byte` buffer.
    *   Pipes the buffers dynamically as `StreamOutputResponse` chunks back to the client over a gRPC stream channel.
    *   Ensures no zombie processes remain by actively calling `Wait()` once the pipe readers reach `io.EOF`.
*   **Unix Signal Delivery (`SendSignal`)**:
    *   Translates client request IDs into active OS Process IDs (PIDs) via `os.FindProcess`.
    *   Sends native Unix signal calls (like `syscall.SIGINT` to abort or `syscall.SIGKILL` to force-terminate) directly to the OS-level process.

---

### C. File and Disk IO: `internal/agent/filesystem.go`
Provides file transfer and directory manipulation capabilities.

*   **Shared Volume Namespace**: The sandbox daemon is configured to share a mounting workspace volume (`/workspace` emptyDir) with the runtime container.
*   **Optimized Byte Write (`WriteFile`)**:
    *   Automatically ensures parent directories exist (`os.MkdirAll`) before opening the target descriptor.
    *   Writes raw binary payloads (`bytes` content) directly to disk, preserving permissions (`0644`).
*   **Binary Stream Reads (`ReadFile`)**:
    *   Reads file content in one clean byte pass, raising standard gRPC `NotFound` (code 5) statuses if files are missing.
*   **Stat and Metadata (`StatFile`/`ListFiles`)**:
    *   Leverages Go's standard file system utilities (`os.Stat`, `os.ReadDir`) to extract file sizes, directory markers, and modification timestamps (`timestamppb`).

---

### D. Stateful Interactive Python: `internal/agent/jupyter.go`
This is the most innovative component of the portable backend. Rather than running separate scripts continuously (which wipes variables and library imports between calls), this module bridges code execution to a persistent local Python kernel.

*   **Dynamic Initialization (`ensureJupyterRunning`)**:
    *   Spawns a background local tokenless Jupyter Server daemon (`jupyter server`) inside the container when the first session is requested.
    *   Disables token/password checks (`--ServerApp.token=''`) for secure localhost-only loopback networking inside the Pod.
    *   Polls `/api/kernels` dynamically until the local endpoint is healthy.
*   **Session Allocation (`CreateSession`)**:
    *   Sends a REST request to the local Jupyter API (`/api/sessions`) to allocate a new stateful python execution kernel.
    *   Generates and returns a UUID session identifier.
*   **WebSocket Execution Bridge (`ExecuteCode`)**:
    *   Connects to the local Jupyter Kernel WebSocket channel:
        ```
        ws://127.0.0.1:8888/api/kernels/<kernel-id>/channels
        ```
    *   Translates our gRPC execution requests into JSON Jupyter Kernel execution payloads (`execute_request`) and sends them over the WebSocket.
    *   Runs a real-time read loop on the WebSocket, capturing streamed execution events:
        *   `stream` events: Map directly to stdout/stderr outputs.
        *   `execute_result`/`display_data` events: Capture plaintext outputs and rich image plots (like Matplotlib charts in base64 PNG bytes) and returns them.
        *   `error` events: Capture stack trace slices (`traceback`) and raise them as errors.
        *   `status` idle: Signals that execution completed, closing the loop.

---

### E. Environment Reuse & Reset: `internal/agent/admin.go`
Enables dynamic sub-second sandbox resets, avoiding expensive Kubernetes Pod recreations.

*   **Golden State Extraction (`Setup`)**:
    *   Accepts a URL pointing to a base workspace tarball.
    *   Downloads, extracts, and decompresses the gzip stream directly into `/workspace`, establishing a fresh filesystem.
*   **Cgroup and Process Sanitization (`Clean`)**:
    *   Reads all active processes inside the container's `/proc` namespace.
    *   Iterates through active PIDs and **force-kills (`SIGKILL`) all user-spawned processes**, while carefully skipping:
        *   PID 1 (Container Init)
        *   The Sandbox Agent Daemon's own Go process.
        *   Core system infrastructure (like `systemd` or `containerd` flags).
    *   Wipes the shared `/workspace` directory clean, returning the sandbox instantly to its pristine base state.

---

## 4. Why This Design Beats Legacy Alternatives (like `envd`)

| Feature / Metric | E2B `envd` Approach | `agent-sandbox-agent` Approach |
| :--- | :--- | :--- |
| **System Targets** | Firecracker MicroVMs (Requires complex local patches for containers). | **Native Kubernetes Containers** (Runs inside standard unprivileged Pod namespaces). |
| **Dual REST/gRPC** | Heavy custom connect wrappers. | **gRPC Native with Reflection** (High performance, direct CLI testable out-of-the-box). |
| **Jupyter Integration** | Tunneled externally or run via raw processes. | **First-Class WebSocket Bridge** (Native variable persistence, base64 image plots). |
| **Resource Footprint** | High memory overhead. | **Ultra-lightweight Go Binary** (< 15MB footprint, sub-millisecond process spawning). |
