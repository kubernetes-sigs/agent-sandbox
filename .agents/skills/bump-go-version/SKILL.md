---
name: bump-go-version
description: Bumps the Go version to the latest release across go.mod, tools.mod, and Dockerfiles.
---

# Bump Go Version Skill

## Purpose
This skill guides AI agents to update the Go version used throughout the `agent-sandbox` repository to the latest stable Go release. It ensures consistency across Go module definitions (`go.mod`, `tools.mod`, `toolchain`) and container image base layers (`Dockerfile`).

## Instructions

1.  **Determine the Latest Go Version**:
    *   Run the helper script provided with this skill to fetch the latest stable Go release version string (e.g., `go1.26.3`). Execute this from the repository root:
        ```bash
        ./.agents/skills/bump-go-version/scripts/get-latest-go
        ```
    *   Note the exact version strings needed:
        *   For `go mod edit -go`, use the numeric format `1.x` (e.g., `1.26`).  This is the language version and should not specify a patch version.
        *   For `go mod edit -toolchain`, use the `go1.x.y` format (e.g., `go1.26.3`).
        *   For `Dockerfiles`, identify the corresponding official `golang` image tag (e.g., `golang:1.26.3`).

2.  **Locate Target Files and Context**:
    *   Run the helper script provided with this skill to locate all relevant files and inspect their current `FROM`, `go`, and `toolchain` lines. Execute this from the repository root:
        ```bash
        ./.agents/skills/bump-go-version/scripts/find-files
        ```
    *   The script will output the list of all `go.mod`, `tools.mod`, and `Dockerfile*` files in the repository along with their current version directives. (Note: `site/go.mod` is automatically excluded by the script).

3.  **Update Go Module Directives**:
    *   By default, **only update the `toolchain` directive** in each `go.mod` and `tools.mod` file using `go mod edit`. Do not update the `go` language version directive by default, as bumping the minimum language version forces that requirement onto all downstream consumers of our modules.
    *   Example (Default Workflow):
        ```bash
        go mod edit -toolchain=go1.26.3 go.mod
        go mod edit -toolchain=go1.26.3 tools.mod
        ```
    *   *Optional Language Version Bump*: If the user explicitly requests bumping the Go language version (e.g., when adopting new language features), update the `go` directive using `go mod edit -go=1.x` (specifying only the major/minor version, e.g., `1.26`, without a patch version).
        ```bash
        go mod edit -go=1.26 go.mod
        ```
    *   *CRITICAL NOTE*: **Do NOT update or modify `site/go.mod`**. `site/go.mod` is a Hugo theme configuration file, not a Go code module, and modifying it or running Go tools against it will break the documentation build.

4.  **Update Dockerfiles**:
    *   For each `Dockerfile` (or `Dockerfile.*`) identified by the script, inspect the file for any `FROM golang:<version>` or `FROM --platform=$BUILDPLATFORM golang:<version>` lines.
    *   Update the version tag to match the latest Go release (e.g., change `golang:1.26.2` to `golang:1.26.3`).
    *   Do not modify other base images (e.g., `debian`, `distroless`) unless specifically required.

5.  **Verify and Test**:
    *   After updating the files, run `go mod tidy` in directories containing `go.mod` files to verify module resolution and clean up dependencies.
    *   *CRITICAL NOTE*: **Do NOT run `go mod tidy` inside `site/`**. Running Go's native `go mod tidy` in the Hugo site directory will fail or strip theme dependencies.
    *   Run `make all` (which includes lint(ing), building, and unit testing) from the repository root to ensure the project builds and tests pass successfully with the new Go version.
    *   Ensure no unintended formatting changes or unrelated modifications are introduced.

## Helper Scripts
- [`scripts/get-latest-go`](scripts/get-latest-go): Fetches the latest stable Go release version string.
- [`scripts/find-files`](scripts/find-files): Scans the repository and outputs all `go.mod`, `tools.mod`, and `Dockerfile` files along with their current version context lines (excluding `site/go.mod`).

## References
- [Go Release History](https://go.dev/doc/devel/release)
- Project rules (as documented in [`AGENTS.md`](../../../AGENTS.md) and [`CONTRIBUTING.md`](../../../CONTRIBUTING.md))
