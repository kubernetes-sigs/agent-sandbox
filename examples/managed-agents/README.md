# Managed Agents on Agent Sandbox

**Managed agent platforms** host the agent loop — model, prompts, session
state, event history — as a service, and delegate **tool execution** (bash,
file operations, skills) to infrastructure you run yourself through a work
API. Each subdirectory here bridges one platform's work API to
[Agent Sandbox](https://agent-sandbox.sigs.k8s.io/), so every agent session's
tool calls run in an isolated, warm-pool-claimed, suspend/resume-capable
`Sandbox` pod.

> **Platform work API → controller (poll/dispatch) → one Sandbox per session →
> in-sandbox runner executes tool calls → idle sessions suspended, resumed on
> the next work item.**

## Why Agent Sandbox

A managed-agents bridge needs exactly the properties the `Sandbox` CRD and
its extensions provide. The mapping is one-to-one:

| Platform need | Agent Sandbox feature |
| :--- | :--- |
| One isolated environment per session | One `Sandbox` (pod-backed, singleton) per session, claimed through a `SandboxClaim` |
| Low claim latency on every new session | `SandboxWarmPool` keeps pre-warmed sandboxes ready; a claim binds one in seconds instead of cold-starting a pod |
| Density: idle sessions must not burn nodes | `Suspend` terminates the pod while keeping the `Sandbox` object; `Resume` brings it back on the next work item |
| Session file state must survive suspension | `volumeClaimTemplates` PVCs are retained across suspend/resume and reattached to the new pod |
| A control channel to the in-sandbox daemon | Apiserver-brokered port-forward (no ingress needed), or the sandbox-router for larger fleets |
| Tenant workloads stay declarative | Everything the bridge owns is a CR: `SandboxTemplate`, `SandboxWarmPool`, `SandboxClaim` |

## Shared architecture

Every bridge in this directory has the same three moving parts; only the
platform protocol differs:

```
┌─────────────────────┐   platform work API    ┌─────────────────────────────┐
│ Managed agent       │ ◄───────────────────── │ controller (local process)  │
│ platform            │  claim/ack/lease,      │  ├─ poller loop             │
│ (e.g. Anthropic)    │  tool results          │  ├─ session manager         │
└─────────▲───────────┘                        │  │   (per-session FIFO)     │
          │                                    │  └─ GC loop (idle→Suspend)  │
          │ tool calls executed, results       └───┬──────────────▲──────────┘
          │ streamed back by the runner            │ Go SDK claim │ port-forward
┌─────────┴───────────┐   POST /work, GET /status  │ suspend/     │ (via apiserver)
│ runner (in sandbox, │ ◄──────────────────────────┘ resume       │
│ HTTP daemon)        │                                          │
└─────────────────────┘
```

- **Controller** — polls (or receives) work from the platform and routes it
  to its session. Where the protocol has ack semantics (CMA), the controller
  acknowledges work items so they are not re-delivered; where it does not
  (the Agents API re-publishes a pending connection action until served),
  idempotent consumption replaces acks. One goroutine per session serializes
  that session's items; the first item claims a warm sandbox, later items
  reuse or resume it.
- **Session manager ↔ Sandbox lifecycle** — `EnsureReady` (claim or resume +
  re-establish the port-forward), dispatch the work item, wait for the runner
  to report completion, then let the session go idle.
- **GC loop** — suspends sandboxes idle past a threshold and releases the
  claim (deleting the PVC) past a longer one. Suspension is always safe
  against racing work: the next item simply resumes the sandbox.
- **Runner** — an HTTP daemon inside the sandbox image. It accepts a work
  item and executes the platform's tool calls locally (bash, files,
  skills) — directly, or by supervising the platform's own executor binary
  when the platform ships one — while keeping any lease/heartbeat contract
  alive, and exposes status and idle-time endpoints the controller polls.

What each bridge implements on top of this shape: the platform-specific
**work protocol** (poll/ack/lease or connection semantics, credentials),
the **runner image** (platform SDK or executor + shell/tool dependencies),
and the **manifests** (`SandboxTemplate` + `SandboxWarmPool`).

## Bridges

| Directory | Platform | Work protocol |
| :--- | :--- | :--- |
| [claude-managed-agents](./claude-managed-agents/) | [Claude Managed Agents self-hosted sandboxes](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) | poll → ack → heartbeat lease hand-off to the in-sandbox worker |
| [openai-managed-agents](./openai-managed-agents/) | [OpenAI Agents API self-hosted sandboxes](https://developers.openai.com/api/docs/guides/agents-api/environments/self-hosted) | poll session required actions → start the platform's `codex exec-server` in-sandbox; it connects outbound (WSS), no lease — the connection's lifetime is the state |

## Shared prerequisites

- A Kubernetes cluster with the Agent Sandbox controller **and the extension
  CRDs/controllers** (`SandboxTemplate` / `SandboxWarmPool` / `SandboxClaim`)
  installed — `EXTENSIONS=true make deploy-kind` on kind.
- Egress from sandbox pods to the platform's API endpoint (the runner talks
  to the platform directly).
- Platform credentials for both sides: the controller needs the API-side
  credential (to poll work), and the sandbox pods need the environment-side
  credential (to execute and report), injected from a Secret in the
  `SandboxTemplate`.

Each bridge's own README covers its setup, quick start, and protocol
details.
