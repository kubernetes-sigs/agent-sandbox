# KEP-1496: agtsbx, a docker-like CLI for running one command in a sandbox

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
  - [Open Questions](#open-questions)
- [Scalability](#scalability)
- [Alternatives (Optional)](#alternatives-optional)
<!-- /toc -->

## Summary

`agtsbx run IMAGE COMMAND` creates a throwaway sandbox, runs one command in it,
streams the output, tears the sandbox down and exits with the command's exit
status. It works against a local container engine or a Kubernetes cluster by
talking to the `sandboxd` contract from [KEP-539.2](../539.2-runtime-standardization/README.md).
Tracking PR: [#1496](https://github.com/kubernetes-sigs/agent-sandbox/pull/1496).

## Motivation

Trying agent-sandbox today means installing a cluster, the controller and a
`SandboxTemplate` first. That is a lot of ceremony to answer "does my agent's
code work in a sandbox?". Coding agents are the motivating case: they edit
files and run commands on the user's behalf, which is the work a sandbox
exists to contain.

### Goals

- One command to run a process in an isolated sandbox, with docker-like
  semantics (streamed output, propagated exit status, cleanup on Ctrl-C).
- Identical in-sandbox behavior on every backend, via `sandboxd`.
- No new API, no CRD change, no new module dependencies.

### Non-Goals

- Replacing the SDKs, `SandboxClaim`, `SandboxTemplate` or `SandboxWarmPool`.
- Long-lived or interactive sessions beyond `--keep`.
- Being a general container runtime or orchestrator.

## Proposal

### User Stories (Optional)

- A developer runs `agtsbx run -e ANTHROPIC_API_KEY agents:latest claude -p '...'`
  locally and gets the agent's output, with the agent contained.
- A CI job runs `agtsbx run --runtime kubernetes IMAGE make test` and relies on
  the exit status.

### High-Level Design

A backend only answers "where do I dial `sandboxd`?". Everything after that
(health polling, `ProcessService.Execute`, output streaming, exit status) is
backend-agnostic.

| Backend | Creates | Reaches sandboxd via |
| --- | --- | --- |
| container engine (`docker` and compatible engines) | a container, ports on `127.0.0.1` | the published loopback port |
| `kubernetes` | a core `Sandbox` | a port-forward to the pod |

`--runtime auto` uses the first local engine found and falls back to
`kubernetes`; an explicit `--runtime` is never silently downgraded. The
Kubernetes backend uses the core `Sandbox` API rather than `SandboxClaim`,
because only `Sandbox` accepts an image directly.

Teardown removes only objects labelled `app.kubernetes.io/managed-by: agtsbx`,
so a name collision never deletes a user's own object.

#### API Changes

None. The binary is `bin/agtsbx`, built by `make build-agtsbx`.

#### Implementation Guidance

- `cmd/agtsbx`: entrypoint and signal handling.
- `internal/agtsbx`: `backend.go` (runtime selection), `container.go`,
  `kube.go`, `exec.go` (sandboxd client), `run.go` (orchestration).
- Container backends drive the engine CLI instead of linking an engine SDK.

Security baseline, since `sandboxd` has no authentication of its own:

- Containers: `--cap-drop ALL`, `no-new-privileges`, loopback-only ports.
- Kubernetes: no service account token, `runAsNonRoot`, no privilege
  escalation, all capabilities dropped, reached by port-forward only.
- `-e` values travel in the `ProcessService` request and are never written to
  the engine argv or the `Sandbox` spec.

### Open Questions

1. **Scope.** Should local container orchestration live in this repo, or should
   the project stay on Kubernetes primitives and the client SDKs? If it stays,
   the Kubernetes-only subset could move to an SDK example.
2. **Kubernetes UX.** A raw `Sandbox` plus port-forward bypasses claims,
   templates, warm pools and the router. Should `agtsbx` instead create a
   `SandboxClaim` from a named template, at the cost of requiring a template?
3. **Network isolation.** A raw `Sandbox` has no template-managed
   `NetworkPolicy`, and `sandboxd` binds `0.0.0.0` by default. Options: create
   an owned `NetworkPolicy` per run, or bind `sandboxd` to loopback and
   port-forward only.
4. **Resource limits.** Neither backend sets CPU, memory or PID limits today.
   Options: bounded defaults with overrides, or opt-in flags only.
5. **Engine probing.** `auto` checks that the CLI exists, not that the engine
   is reachable. Should it probe liveness before choosing?

## Scalability

Each run is one container or one `Sandbox`, created and removed by the caller.
There is no controller-side state, watch, or reconcile load beyond the
existing `Sandbox` reconciler handling one more object. `--keep` leaves the
object in place, so it is the caller's job to clean it up.

## Alternatives (Optional)

- **SDK-only.** Keep the Go and Python SDKs as the single entry point. No new
  binary to maintain, but no zero-setup local path and no shell-friendly
  exit-status contract.
- **`SandboxClaim` backend only.** Reuses templates, warm pools and network
  policy, but needs a cluster and a template before anything runs, which is
  the friction this KEP is trying to remove.
- **Separate repository.** Avoids ownership of local-engine orchestration here,
  at the cost of drifting from the `sandboxd` contract.
