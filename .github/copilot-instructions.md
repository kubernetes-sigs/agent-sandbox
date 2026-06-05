# AI Code Review Guidelines

**Project Context:**
`agent-sandbox` is a Kubernetes controller that provides the `Sandbox` CRD: a stateful, singleton, pod-backed workload with a stable identity, intended for AI agent runtimes, dev environments, notebooks, and similar use cases. It is a SIG Apps subproject (`sigs.k8s.io/agent-sandbox`) and follows Kubernetes / `controller-runtime` conventions.

- API group `agents.x-k8s.io/v1beta1`: `Sandbox` (core).
- API group `extensions.agents.x-k8s.io/v1beta1`: `SandboxClaim`, `SandboxTemplate`, `SandboxWarmPool` (opt-in extensions).
- Go module: `sigs.k8s.io/agent-sandbox` (Go 1.26.x, see `go.mod`).

**Project Toolchain & Versions:**
The Go toolchain version targeted by this repository is the value of the `go` directive in `go.mod` at the head of the PR's base branch. Defer to that value as the authoritative target. Do **not** suggest lowering the targeted Go version, dropping support for newer language features that compile cleanly under it, or adding compatibility shims for older toolchains the repo has already moved past. If a PR introduces a `go` bump, evaluate the bump on its own merits (motivation, blast radius) — not by pattern-matching to "older is safer". Treat the version set in `go.mod` as a deliberate maintainer decision unless the PR is itself changing it.

**Lint Policy:**
This repository's binding style and correctness gate is whatever lint config exists at the head of the PR's base branch (e.g. `.golangci.yml`, `.golangci.yaml`, `.golangci-kal.yml`, or absence of one). If the repo has not opted into a particular linter or stylistic rule, do **not** introduce that rule via review comments. Bias toward stylistic suggestions only when:
- the rule is enforced by the repo's existing lint config, **or**
- the change introduces a clear bug (not a clear style preference), **or**
- the file already follows a local convention and the new code visibly diverges from it.

If the repo's lint gate (`make lint-go` and `make lint-api`, which wrap `./dev/tools/lint-*`) and `go test` all pass and no lint config flags the line, treat residual style as author preference rather than a review-blocking concern.

**Scope of Review:**
Focus on substantive findings tied to the lines the PR actually changes — logic bugs, security issues, controller-runtime misuse, API/contract breaks, missing tests for the new behavior. In particular:
- Do **not** flag style issues in pre-existing code that the PR happens to move or re-format mechanically.

When in doubt between flagging a marginal nit and staying silent: stay silent. Each comment costs the contributor attention, and a noisy review erodes the signal of the substantive findings.

**Specific Conventions & Gotchas:**
Pay special attention to these project-specific rules:
*   **Label Values**: Do NOT put full resource names in label values (to avoid exceeding size limits).
*   **Preview Features**: Do NOT use annotations for alpha/preview features. Advise using new API fields instead.
*   **Mutating Spec**: The `spec` of the primary Custom Resource (CR) being reconciled is user-owned and should not be modified and saved back to the API server by the reconciler. This avoids mutating user intent. Controllers may, however, create and update the `spec` of **secondary or target** objects (for example, the HPA controller updating a Deployment's `spec.replicas`).
*   **Status Properties**: Prefer `conditions` instead of a `phase` enum for tracking state.
*   **Zero vs. Unset**: Suggest using pointers for fields where distinguishing between zero and unset is important.
*   **Booleans**: Advise against booleans for fields that might evolve to have more states in the future.
*   **Error Wrapping**: Always wrap errors with context (`fmt.Errorf("...: %w", err)`). Surface meaningful conditions on the resource status rather than swallowing errors.
*   **Structured Logging**: Use `logr.Logger` from controller-runtime (`log.FromContext(ctx)`). Always use structured key/value pairs; never use `fmt.Sprintf` for log messages.
*   **Docs Site Mounts**: Root files like `README.md`, `CONTRIBUTING.md`, and client READMEs are mounted directly to the public Hugo website (`https://agent-sandbox.sigs.k8s.io`). Treat edits to these files as formal public documentation.
*   **Python SDK (`clients/python`)**: Enforce Python 3.10+ idioms, Pydantic models for data structures, and maintain architectural parity between sync modules and their async siblings (e.g., `sandbox_client.py` vs `async_sandbox_client.py`), avoiding unintended drift. Note the three distinct names: directory `agentic-sandbox-client`, package `k8s_agent_sandbox`, PyPI wheel `k8s-agent-sandbox`.

**CLA Reminder:**
When you provide code suggestions in a review, add a reminder at the end of your comment that the contributor should **not** click the "Commit suggestion" button in the GitHub UI. Explain that doing so adds the AI bot as a co-author, which breaks the Kubernetes CLA check as bots cannot sign it. Advise them to apply the suggestion locally instead.

**Tone:**
Constructive, empathetic, and professional. Always explain the reasoning behind your suggestions.
