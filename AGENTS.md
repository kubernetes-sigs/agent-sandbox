# AGENTS.md

Welcome, AI assistant! This file provides context and instructions for you to contribute effectively to the `agent-sandbox` project.

## Project Overview
`agent-sandbox` enables management of isolated, stateful, singleton workloads (like AI agent runtimes) on Kubernetes using a `Sandbox` CRD.

### Codebase Structure
- **`api/`**: Core API definitions (`Sandbox` CRD).
- **`controllers/`**: Controller for the core `Sandbox` resource.
- **`extensions/`**: Additional CRDs and controllers (`SandboxClaim`, `SandboxTemplate`, `SandboxWarmPool`).
- **`cmd/`**: Entry points for the application (controller manager).
- **`clients/`**: Client libraries for interacting with the API (Go, Python, K8s).
- **`internal/`**: Internal libraries (lifecycle management, metrics, version).
- **`examples/`**: Example configurations for resources.
- **`docs/`**: Project documentation.
- **`site/`**: Website source files.
- **`.agents/skills/`**: Specialized instructions for you!

## Setup & Verification Commands

Useful commands for development and testing:

### Environment Setup
- Create a local `kind` cluster: `make deploy-kind`
- Delete the local `kind` cluster: `make delete-kind`

### Build & Test
- Install dependencies: `go mod download`
- Build controller binary: `make build` (creates `bin/manager`)
- Run unit tests: `make test-unit`
- Run e2e tests: `make test-e2e`
- Run linter: `make lint-go` and `make lint-api`

## Agent Skills
You **MUST** discover and load the skills in the [`.agents/skills/`](.agents/skills/) directory before proceeding with tasks involving code modification or API design. These directories contain specific instructions and references (e.g., for code style and API conventions) that you must follow.
