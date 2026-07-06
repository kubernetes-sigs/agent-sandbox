# KEP-539.2: Standardizing Sandbox Runtime Interfaces

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
  - [Should We Support Both?](#should-we-support-both)
- [Conformance](#conformance)
  - [1. Protocol Adherence](#1-protocol-adherence)
  - [2. Conformance Levels](#2-conformance-levels)
  - [3. Automated Conformance Testing](#3-automated-conformance-testing)
- [High-Level Design](#high-level-design)
- [Scalability](#scalability)
- [Extensions](#extensions)
  - [Reusable Sandboxes (Setup &amp; Cleanup)](#reusable-sandboxes-setup--cleanup)
    - [Implementation Considerations for Reusability](#implementation-considerations-for-reusability)
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



---

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

---

### Should We Support Both?

**We propose a hybrid or pluggable approach rather than strictly choosing one.**

1. **Protocol-First Definition:** We should define the *capabilities* and *data models* abstractly.
2. **Pluggable Transports:** 
   * We can provide an **OpenAPI/REST** interface as the default, high-compatibility entry point for most users and languages.
   * We can support a **gRPC** interface for performance-critical workloads or when advanced streaming (like `Watch` or interactive PTY) is required.
3. **Sidecar Model:** By using a sidecar model in our Kubernetes Pods, users could choose to inject an `execd` (REST) sidecar or an `envd` (gRPC) sidecar depending on their SDK and performance needs, while keeping the underlying `agent-sandbox` CRD management identical.

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

## Alternatives

- **Status Quo:** Continue letting examples define their own ad-hoc HTTP servers. This leads to fragmentation and prevents building a robust, reusable client SDK.
- **Strictly gRPC:** Win on performance but lose on simplicity and ease of adoption.
- **Strictly REST:** Win on simplicity but limit advanced use cases requiring high-performance streaming.
