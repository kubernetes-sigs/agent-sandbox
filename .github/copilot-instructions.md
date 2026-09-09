# AI Code Review Guidelines

**Project Context & Architecture:**
Refer to [AGENTS.md](../AGENTS.md) for full project background, module layout, toolchain versions, and core conventions.

**Scope of Review:**
Focus strictly on substantive findings tied to lines the PR actually modifies — logic bugs, concurrency issues, security vulnerabilities, controller-runtime misuse, API contract breaks, and missing tests for new behavior.
- Do NOT flag style issues in pre-existing code that the PR touches mechanically.
- When in doubt between flagging a marginal nit and staying silent: **stay silent**. Do not introduce review fatigue.

**Toolchain & Lint Policy:**
- Defer to the `go` directive in `go.mod` at the base branch head as the authoritative target. Do not suggest lowering the Go version or adding compatibility shims for older toolchains.
- The binding style and correctness gate is the repo's lint configuration (`make lint-go`, `make lint-api`). If existing linters and tests pass without flagging a line, treat residual style as author preference.

**CLA Safety Reminder:**
When offering code suggestions, always remind contributors **not** to click "Commit suggestion" in the GitHub UI (which adds the AI bot as co-author and breaks the CNCF/Kubernetes CLA check), and to apply the change locally instead.

**Tone:**
Succinct, constructive, and direct. Explain technical rationale clearly without conversational filler.

