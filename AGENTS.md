# AGENTS.md

Guidance for AI coding agents working in this repository. Human contributors should also read [CONTRIBUTING.md](CONTRIBUTING.md), [docs/development.md](docs/development.md), and [docs/testing.md](docs/testing.md), which are the source of truth.

## Project summary

`agent-sandbox` is a Kubernetes controller that provides the `Sandbox` CRD: a stateful, singleton, pod-backed workload with a stable identity, intended for AI agent runtimes, dev environments, notebooks, and similar use cases. It is a SIG Apps subproject (`sigs.k8s.io/agent-sandbox`) and follows Kubernetes / `controller-runtime` conventions.

- API group `agents.x-k8s.io/v1alpha1`: `Sandbox` (core).
- API group `extensions.agents.x-k8s.io/v1alpha1`: `SandboxClaim`, `SandboxTemplate`, `SandboxWarmPool` (opt-in extensions).
- Go module: `sigs.k8s.io/agent-sandbox` (Go 1.26.x, see [go.mod](go.mod)).

## Repository layout

| Path | What lives there |
| --- | --- |
| [api/v1alpha1/](api/v1alpha1/) | Core `Sandbox` types and kubebuilder markers. |
| [extensions/api/v1alpha1/](extensions/api/v1alpha1/) | `SandboxClaim`, `SandboxTemplate`, `SandboxWarmPool` types. |
| [controllers/](controllers/) | Core `Sandbox` reconciler + tests. |
| [extensions/controllers/](extensions/controllers/) | Reconcilers for the extension CRDs. |
| [cmd/agent-sandbox-controller/](cmd/agent-sandbox-controller/) | Controller manager entrypoint. |
| [internal/](internal/) | Shared internals: `lifecycle`, `metrics`, `version`. Not importable by external consumers. |
| [k8s/](k8s/) | Generated CRDs ([k8s/crds/](k8s/crds/)), RBAC, controller manifests. |
| [clients/k8s/](clients/k8s/) | **Generated** Kubernetes-style clientset, listers, informers (output of `dev/tools/client-gen-go.sh`). Do not hand-edit. |
| [clients/go/](clients/go/) | Hand-written high-level Go SDK that wraps the `SandboxClaim` lifecycle and exposes Gateway / port-forward / direct connectivity. Editable. |
| [clients/python/agentic-sandbox-client/](clients/python/agentic-sandbox-client/) | Hand-written Python SDK. Directory is named `agentic-sandbox-client` but the package publishes to PyPI as **`k8s-agent-sandbox`** — that's the name to import in docs and examples. |
| [examples/](examples/), [extensions/examples/](extensions/examples/) | Runnable sample manifests and demo apps. |
| [test/e2e/](test/e2e/), [test/benchmarks/](test/benchmarks/) | End-to-end and benchmark suites (Go-driven; some scenarios shell out via the Python SDK). |
| [dev/tools/](dev/tools/) | Repo tooling (lint, generate, deploy-kind, release scripts). Most `make` targets shell out here. |
| [dev/ci/](dev/ci/) | Prow presubmit/periodic scripts. |
| [docs/](docs/) | Development, testing, configuration docs and KEPs ([docs/keps/](docs/keps/)). |
| [site/](site/) | Hugo + Docsy source for https://agent-sandbox.sigs.k8s.io. Many pages are thin wrappers that `include-file` from the repo via mounts in [site/hugo.yaml](site/hugo.yaml) — see "Docs site mounts" below. Native page sources (lifecycle, snapshots, use-cases, runtime-templates, getting started, etc.) live only here. |

When in doubt about ownership, check the nearest `OWNERS` file.

## Build, test, lint

All standard tasks go through the [Makefile](Makefile). Prefer `make` targets over invoking tools directly so CI and local runs stay consistent.

- `make all` — runs `fix-go-generate`, `build`, `lint-go`, `lint-api`, `test-unit`, `toc-verify`. Run this before sending a PR.
- `make build` — compiles `bin/manager` from `cmd/agent-sandbox-controller`.
- `make test-unit` — runs Go unit tests with `-race` enabled and the Python unit test suites via `dev/tools/test-unit`.
- `make test-e2e` / `make test-e2e-race` — e2e suite against a kind cluster (much slower; e2e is not raced by default).
- `make lint-go` / `make fix-go` — `golangci-lint` (config in [dev/tools/.golangci.yaml](dev/tools/.golangci.yaml)).
- `make lint-api` / `make fix-api` — KAL API linter for kubebuilder tags and CRD conventions.
- `make toc-verify` / `make toc-update` — keep markdown TOCs in sync.
- `make deploy-kind` — create a local kind cluster named `agent-sandbox`, build images, deploy the controller, and write the kubeconfig to `bin/KUBECONFIG` (which the e2e suite expects). `EXTENSIONS=true make deploy-kind` to include the extension controllers; `CONTROLLER_ARGS="..."` passes flags to the controller; `CONTROLLER_ONLY=true` builds/pushes only the controller image (skipping example sidecars). `make delete-kind` tears it down.

After editing anything in [api/](api/), [extensions/api/](extensions/api/), or kubebuilder markers in [controllers/](controllers/) / [extensions/controllers/](extensions/controllers/), run `make all` (or at least `make fix-go-generate`) to regenerate CRDs in [k8s/crds/](k8s/crds/), RBAC manifests in [k8s/](k8s/), deepcopy code, and the typed clients. The exact directives live in [codegen.go](codegen.go). Commit the regenerated output alongside the source change — never hand-edit `zz_generated_*.go`, `*.generated.yaml`, or files under [clients/k8s/](clients/k8s/).

## Docs site mounts

The Hugo site at [site/](site/) mounts these repo paths into `assets/additional/` and surfaces them on https://agent-sandbox.sigs.k8s.io. **Edits to these files change the public docs site**, so treat them as documentation, not just internal READMEs:

- [README.md](README.md) → `/docs/overview/`
- [CONTRIBUTING.md](CONTRIBUTING.md) → `/docs/contribution-guidelines/`
- [docs/testing.md](docs/testing.md) → `/docs/contribution-guidelines/testing/`
- [clients/go/README.md](clients/go/README.md) → `/docs/go-client/`
- [clients/python/agentic-sandbox-client/README.md](clients/python/agentic-sandbox-client/README.md) → `/docs/python-client/`
- Many `examples/*/README.md` files (and some `extensions/examples/*/README.md` files) → pages under `/docs/use-cases/examples/` and `/docs/runtime-templates/`

If you change one of these, preview the rendered output (`hugo server` from [site/](site/) — Hugo extended is required; check `module.hugoVersion` in [site/hugo.yaml](site/hugo.yaml) for the declared minimum, but in practice run a recent stable Hugo release). Do not edit the generated `site/public/` or `site/resources/` directories.

## Coding conventions

- Idiomatic Go, controller-runtime patterns. Reconcile loops must be idempotent and safe under retries; do not assume single execution.
- Comments explain **why**, not **what**. Identifier names should carry the "what."
- Keep changes scoped to the task. No drive-by refactors, speculative abstractions, or unrelated formatting churn — bundle one concern per PR.
- Prefer extending existing files over adding new ones. Do not create new top-level directories without discussion.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`); surface meaningful conditions on the resource status rather than swallowing.
- Concurrency: respect `context.Context` cancellation; avoid goroutines without lifetime ownership; protect shared state.
- Logging: use `logr.Logger` from controller-runtime (`log.FromContext(ctx)`); use structured key/value pairs, not `fmt.Sprintf`.
- API changes are versioned (`v1alpha1`). Treat any user-visible field, label, or annotation rename as a breaking change — discuss in an issue or KEP first ([docs/keps/](docs/keps/)).
- Match existing kubebuilder marker style; required vs optional, default values, and validation belong on the type, not in the controller.

## Python SDK conventions

The Python SDK lives at [clients/python/agentic-sandbox-client/](clients/python/agentic-sandbox-client/) and is the second supported client surface alongside the Go SDK. It has its own conventions; treat it as a Python project, not as appendage code.

- **Three names, do not confuse them:** repo directory `agentic-sandbox-client`, importable package `k8s_agent_sandbox` (underscore), PyPI distribution `k8s-agent-sandbox` (hyphen). Examples and docs install with `pip install k8s-agent-sandbox` and import `from k8s_agent_sandbox import ...`.
- **Supported Python:** `>=3.10` ([pyproject.toml](clients/python/agentic-sandbox-client/pyproject.toml)). Do not use syntax or stdlib features beyond 3.10 unless you also raise the floor deliberately.
- **Sync/async parity:** every public sync module (`sandbox_client.py`, `sandbox.py`, `k8s_helper.py`, `connector.py`, `files/filesystem.py`, `commands/command_executor.py`) has a `async_*` sibling. When you change one, change the other — drift between sync and async is a real bug surface here. Async-only deps (`httpx`, `kubernetes_asyncio`) belong behind the `async` optional extra, not in the base `dependencies` list.
- **Optional extras structure:** the project ships three extras — `async`, `test`, `tracing`. New optional functionality should follow the same pattern (extra in `[project.optional-dependencies]`, lazy import inside the relevant module) rather than adding to the base install footprint. The base deps are intentionally minimal: `kubernetes`, `requests`, `pydantic`.
- **Use `pydantic` for data models.** Configuration and wire-format types belong in [k8s_agent_sandbox/models.py](clients/python/agentic-sandbox-client/k8s_agent_sandbox/models.py); extend those rather than passing free-form dicts.
- **File headers and docstrings:** every `.py` file in the package starts with the Apache-2.0 boilerplate header followed by a module docstring (triple-quoted, one paragraph minimum). Match the existing style — `git show` an existing file if unsure.
- **Tests:** unit tests live in [k8s_agent_sandbox/test/unit/](clients/python/agentic-sandbox-client/k8s_agent_sandbox/test/unit/) and run via pytest. They are wired into `make test-unit` — [dev/tools/test-unit](dev/tools/test-unit) creates an isolated venv, runs `pip install -e .[test]`, and writes JUnit XML to `bin/python-k8s-agent-sandbox-junit.xml`. Do not invent a different layout. New behavior needs a unit test in the existing suite.
- **`sandbox-router/` is a separate app**, not part of the published wheel. `pyproject.toml` includes only `k8s_agent_sandbox*`. The router has its own `requirements.txt` and its own pytest run in CI. Don't import from the SDK package into the router or vice versa unless you mean it.
- **Versioning** is handled by `setuptools_scm` with `root = "../../.."` (the repo root). The generated `VERSION` file is gitignored — never commit it.
- **No enforced formatter or linter** at the time of writing — there is no ruff/black/mypy config in the repo. **Match the style of the file you are editing**; do not reformat unrelated code, and do not add a tool config as part of an unrelated PR.

## Tests

- Unit tests live next to the code they cover (`*_test.go`) and run under `make test-unit`. New behavior needs a unit test.
- Reconciler tests use envtest-style fixtures in [controllers/](controllers/) and [extensions/controllers/](extensions/controllers/); follow the existing harness rather than inventing a new one.
- E2E tests live in [test/e2e/](test/e2e/) and run against a kind cluster created by `make deploy-kind`. Use them for behavior that crosses the controller/apiserver boundary.
- Benchmarks: [test/benchmarks/](test/benchmarks/), runnable via `make test-e2e-benchmarks`.
- When fixing a bug, add a regression test that fails without the fix.

## Pull requests and CI

- This is a Kubernetes SIG project: contributors must have a signed [CNCF CLA](https://git.k8s.io/community/CLA.md). PRs without one are not reviewed.
- CI runs through Prow (configured in [kubernetes/test-infra](https://github.com/kubernetes/test-infra)). The `k8s-ci-robot` merges PRs once they have `lgtm` + `approve` and presubmits pass. Default merge mode is squash; use `/label tide/merge-method-rebase` only when distinct commits matter.
- A bot may auto-assign GitHub Copilot as a first-pass reviewer. **Never click "Commit suggestion" in the GitHub UI** — that adds Copilot as a co-author, and Copilot cannot sign the Kubernetes CLA, so the CLA check will fail and block the PR. Instead: read the suggestion, apply the change manually in your local checkout, and push it as a normal commit authored by you.
- Inactive PRs go stale after 30 days and close after 15 more. Reopen freely if you return to the work.
- Keep PR titles short and conventional; the body should explain motivation and link any issue or KEP.

## Things to avoid

- Do not commit binaries, `.venv/`, generated kubeconfigs (e.g. `bin/KUBECONFIG`), or anything under `dev/tools/tmp`.
- Do not modify hand-written page sources under [site/content/](site/content/) (e.g., `sandbox/lifecycle`, `sandbox/snapshots`, `use-cases/*`) unless the task is explicitly about docs. Note: the repo-root files listed in "Docs site mounts" above are part of the docs site too — edit them with that in mind.
- Do not commit anything under [site/public/](site/) or `site/resources/` — those are Hugo build artifacts.
- Do not bypass hooks (`--no-verify`), skip `gofmt`, or disable lint rules to make CI pass — fix the underlying issue.
- Do not introduce new external runtime dependencies without strong justification; the controller image is `gcr.io/distroless/static-debian13:nonroot` and stays small.
- Do not change `OWNERS` files, release tooling under [dev/tools/release*](dev/tools/), or anything in [k8s/](k8s/) by hand outside of regeneration flows.

## Useful pointers

- Architecture overview, motivation, and install instructions: [README.md](README.md).
- Roadmap: [roadmap.md](roadmap.md).
- Configuration / scale tuning: [docs/configuration.md](docs/configuration.md).
- Deep design proposals: [docs/keps/](docs/keps/) (template at [docs/keps/NNNN-template/](docs/keps/NNNN-template/)).
- Slack: `#agent-sandbox` on Kubernetes Slack for project work; `#sig-apps` for broader SIG Apps discussion. Mailing list: [SIG Apps](https://groups.google.com/a/kubernetes.io/g/sig-apps). New to k8s Slack? Get an invite at https://slack.k8s.io/.
