# KEP-539: Establish gRPC and Protobuf Contract for Agent Sandbox Runtime

## Summary

Currently, the agent-sandbox project has independent client implementations in Go and Python. There are more upcoming PRs to also include Typescript client implementation as well. To ensure interoperability, reduce maintenance overhead, and provide a definitive "source of truth" for the sandbox API, this KEP proposes establishing a common contract using Protocol Buffers (Protobuf) and gRPC. Furthermore, it proposes a unified, language-agnostic server daemon (`sandboxd`) to run inside all sandbox environments. It also outlines the standardization of behavioral expectations, tooling, and CI.

## Motivation

Maintaining independent client implementations in different languages naturally leads to "implementation drift." For example, the Python client might support a feature (copy files for example) that the Go client handles differently or doesn't support at all. A unified gRPC contract prevents this drift, enforces consistent error handling, and significantly streamlines cross-language development by generating the client boilerplate from a single schema.
Additionally, implementing the server-side of this contract across different sandbox images (e.g., Python, Node) duplicates effort. A unified server binary eliminates this duplication and guarantees the sandbox behaves consistently regardless of the runtime environment.

## Goals

- **Define the core service interface** (`AgentSandboxRuntimeService`) via Protocol Buffers.
- **Standardize Request/Response messages** for agent lifecycle management.
- **Establish common error codes** using standard gRPC status codes (e.g., `INVALID_ARGUMENT` for malformed paths).
- **Develop a unified Sandbox Daemon** (`sandboxd`) in Go to handle requests inside the sandbox container.
- **Document unwritten behavioral expectations** (path validation, response size limits, OTel scoping, and retry policies).
- **Integrate tooling & CI** using `buf` for linting, breaking change detection, and automated code generation.

## Proposal

### 1. Protobuf Definition (`/proto`)
The primary interface will be centralized in a `/proto` directory defining the `AgentSandboxRuntimeService`. This schema will serve as the single source of truth for both Python and Go clients, ensuring identical payload structures and standard gRPC error mappings.

### 2. Behavioral Specification (`CONFORMANCE.md`)
Beyond the interface definition, a shared document (e.g., `CONFORMANCE.md` or `spec/behavior.md`) will capture critical behavioral expectations that cannot be easily expressed in a `.proto` file. This specification will define:

- **Path Validation:** Strict rules dictating the use of absolute vs. relative paths to prevent directory traversal attacks within the sandbox.
- **Response Size Limits:** Maximum byte sizes for stdout/stderr streams or file transfers to prevent out-of-memory (OOM) errors in the controller.
- **OTel Scoping:** Standardized attribute keys for OpenTelemetry (e.g., `sandbox.id`) to ensure consistent distributed tracing across both Go and Python runtimes.
- **Retry Policy:** Default exponential backoff parameters to handle transient network failures gracefully.

### 3. Tooling & CI
To ensure the Protobuf definitions remain clean, backward-compatible, and consistently applied, we will adopt standard ecosystem tooling:

- **Linting:** Integrate `buf` for linting and breaking change detection on the `.proto` files.
- **Code Generation:** Create generation scripts to automatically compile and sync the generated code to the respective `/clients` directories for both Go and Python.

### 4. Unified Sandbox Daemon (`sandboxd`)
To avoid duplicating the server implementation for each environment (e.g., a Python server for the Python sandbox, a Node server for the TS sandbox), we will develop a single `sandboxd` server written in Go.
- **Static Compilation:** The daemon will be built with `CGO_ENABLED=0` to produce a completely static binary.
- **Universal Compatibility:** This static binary can be injected (via Docker `COPY`) into any base image (Alpine, Debian, Ubuntu) without worrying about `glibc` or other dependency mismatches.
- **Single Source of Truth:** `sandboxd` will be solely responsible for executing the commands, managing filesystem boundaries (Path Validation), and emitting OTel metrics, enforcing the `CONFORMANCE.md` rules at the lowest level.

### 5. "Bring Your Own Daemon" (Extensibility)
While `sandboxd` provides a robust, zero-dependency default, the gRPC architecture inherently supports custom server implementations. If a user requires a specialized runtime environment (e.g., an in-memory Python server pre-loaded with heavy ML models), they can bypass `sandboxd`. By generating server stubs from the standard `.proto` files in their language of choice, users can build and run a custom daemon inside their container. As long as the custom daemon adheres to the Protobuf contract and the `CONFORMANCE.md` specifications, the SDK clients will interface with it seamlessly.

## Implementation Plan

### Phase 1: RFC Phase
- Submit an initial PR containing the `.proto` file definition, `CONFORMANCE.md`, and the architectural plan for `sandboxd`.
- Review and iterate on the API design with stakeholders.

### Phase 2: Validation
- Develop the initial Go-based `sandboxd` and containerize it within a base Alpine image.
- Update the Go and Python client codebases.
- Verify that both clients can successfully communicate with `sandboxd` and map accurately to the behavioral spec without feature regressions.

### Phase 3: Automation
- Integrate a GitHub Action to run `buf lint` and breaking change detection automatically on PRs.
- Setup automated workflows to verify generated code is up-to-date in the `/clients` directories.

## Additional Context

Establishing this contract early in the sandbox lifecycle will heavily reduce technical debt. By unifying the networking layer now, we can ensure future features (like bi-directional streaming for exec sessions) are implemented simultaneously and consistently across all supported languages.
