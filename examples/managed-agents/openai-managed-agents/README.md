# OpenAI Managed Agents on Agent Sandbox

Run [OpenAI Agents API self-hosted sandbox](https://developers.openai.com/api/docs/guides/agents-api/environments/self-hosted)
work — the bash, file, and local-MCP tool calls of a session driven by
OpenAI's hosted Codex harness — inside `Sandbox` pods. A local controller
polls the Agents API for sessions that need an environment connection,
claims one warm sandbox per session, starts the session's executor
(`codex exec-server`) through the in-sandbox runner daemon, and suspends
idle sandboxes; the executor connects out to the platform directly and
streams tool results back without passing through the controller.

This is one bridge of the [managed-agents](../../managed-agents/README.md)
example collection; that README explains how these bridges map onto Agent
Sandbox in general. OpenAI names this product the **Agents API** (public
beta since 2026-09-10); this directory follows the collection's
`<platform>-managed-agents` naming. Unlike the CMA bridge, the platform
ships its own executor binary, so the runner supervises `codex
exec-server` instead of executing tool calls in-process, and the protocol
has no work queue and no leases: a session publishes an
`environment_connection` required action, and the executor holds an
outbound WebSocket for the session's life.

## Agents API work protocol

The contract this example implements end to end:

1. `cmd/agent` creates a session with `environment.type = "self_hosted"`,
   working directory `/workspace`, and the metadata marker
   `managed-by=agent-sandbox-example` (the controller only ever touches
   sessions carrying it), then sends the initial prompt.
2. The controller's reconciler lists managed sessions and re-reads each
   session showing an `environment_connection` required action before
   acting — the list can be stale, and only a connection action (never a
   `function_call`) may allocate a sandbox.
3. The session manager claims a warm sandbox (`SandboxClaim` against
   `oma-runner-pool`, with a deterministic claim name derived from the
   session ID so a restarted controller adopts the existing claim),
   port-forwards to the runner on :8080, and POSTs `{session_id,
   environment_id, remote_url, attempt_id}` to `/executor`. The remote URL
   is a capability: it is handed to the executor and never logged.
4. The runner starts `codex exec-server --remote <url> --environment-id
   <id>` and supervises it. The executor connects out to
   `wss://codex-cloud-environments.chatgpt.com`; the harness drives tool
   calls from there. The controller waits for the connection (the runner's
   ready marker, cross-checked against the required action disappearing)
   and then polls the session's turns until the turn settles.
5. After 30s idle (configurable) — and only if the platform confirms no
   active turn and no pending action — the controller stops the executor
   gracefully and suspends the sandbox. The pod terminates but the
   workspace PVC survives; the next turn resumes the sandbox into a **new
   pod** that reattaches the volume, and the executor reconnects **with the
   same remote URL**, which the platform requires to be reused unchanged.
   After 30m idle the claim is deleted entirely (a later turn then starts
   with an empty workspace).

Failure semantics follow the connection rather than a lease: a start
request whose response is lost (transport error, 5xx) is *uncertain* — the
executor may be starting, so lifecycle cleanup stays off for a bounded
window. A runner `409` means the sandbox is bound to a different session;
because that binding is long-lived, the controller abandons and re-claims
instead of retrying. Two failed connection attempts put the session in a
terminal `executor_failed` state (GC still releases its sandbox on
schedule). A crashed executor is never auto-restarted in-sandbox: the
platform re-publishes the connection action and the controller reconnects
with a fresh attempt. Reconnection never replays timed-out input, so the
controller never re-sends prompts; it reads turn outcomes from the API. A
404 on a managed session (the application deleted it) releases the sandbox
immediately, because deleting an Agents API session does not reclaim
self-hosted compute.

## Prerequisites

1. A Kubernetes cluster with the Agent Sandbox controller **and the
   extension CRDs/controllers** installed (`EXTENSIONS=true make
   deploy-kind` for kind).
2. OpenAI credentials, split by trust level as the platform's security
   guidance requires:
   - **Application-side API key** (`OPENAI_API_KEY`, scopes
     `api.agents.read`, `api.agents.write`, `api.responses.write`) for the
     controller and `cmd/agent`. It must never reach a sandbox:
     agent-generated code can read the pod environment.
   - **Restricted environment key** for the sandbox pods, created in the
     platform dashboard with every other permission set to None, injected
     as the `oai-env-key` Secret (key `CODEX_API_KEY`).
3. Go 1.26+, Docker, and [kind](https://kind.sigs.k8s.io/) for the quick
   path.
4. Cluster egress from sandbox pods to `api.openai.com` and
   `wss://codex-cloud-environments.chatgpt.com` (the executor connects out
   directly).

## Quick start

The commands below target the kind cluster `run-test-kind.sh` deploys to.
If your current kubeconfig context is different, add `--context
kind-${KIND_CLUSTER_NAME:-agent-sandbox}` to every `kubectl` and `go run
./cmd/controller` command.

### 1. Create the credentials Secret

The `apply` flow works both on first setup and when replacing the
credential-free placeholder that `run-test-kind.sh` creates:

```bash
kubectl --context "kind-${KIND_CLUSTER_NAME:-agent-sandbox}" -n "${SANDBOX_NAMESPACE:-default}" \
  create secret generic oai-env-key \
  --from-file=CODEX_API_KEY=/path/to/environment-key \
  --dry-run=client -o yaml | \
kubectl --context "kind-${KIND_CLUSTER_NAME:-agent-sandbox}" -n "${SANDBOX_NAMESPACE:-default}" apply -f -
```

Pods pick the new key up when they are recreated; existing warm sandboxes
keep the old value until they are recycled (step 2 recycles them).

### 2. Build and deploy the warm pool

```bash
./run-test-kind.sh
```

The script builds `oma-runner:v1` (build context: the repo root), loads it
into the kind cluster named by `KIND_CLUSTER_NAME` (default
`agent-sandbox`), applies `template.yaml` and `warmpool.yaml`, and waits
for one warm sandbox. Without the Secret it creates a placeholder and
warns — enough for the credential-free link-level test below, not for real
sessions.

On a non-kind cluster, push the image to a registry and change `image:` in
`template.yaml` (keeping `imagePullPolicy: IfNotPresent` or switching to
`Always`).

With `E2E=true`, the script points the template at `fakeexec` — a
same-flag stand-in for the executor baked into the image — and drives the
full Kubernetes-side lifecycle without OpenAI credentials: warm claim,
runner protocol (start/duplicate/conflict), suspend with pod termination,
resume into a new pod (UID-checked), workspace PVC retention, and release:

```bash
E2E=true ./run-test-kind.sh
```

The e2e run leaves `oma-runner-template` patched to `fakeexec`. Before
starting the controller for real sessions, run `./run-test-kind.sh` again
without `E2E=true`: it re-applies `template.yaml` (restoring the real
executor configuration) and recreates the warm sandboxes from it.

### 3. Start the controller

```bash
export OPENAI_API_KEY="sk-..."
go run ./cmd/controller --context kind-agent-sandbox
```

### 4. Start a session

In another terminal, from this directory — the key must be exported in
this terminal too:

```bash
export OPENAI_API_KEY="sk-..."
go run ./cmd/agent --prompt "Run 'uname -a' in bash and show the output."
```

The agent command prints the session ID and a platform URL. Watch the
controller log the claim, the executor connection, and — after the turn
settles and 30s pass — the suspension:

```bash
kubectl --context kind-agent-sandbox get sandbox -w
```

To see the resume path reuse the session's workspace, send a follow-up
prompt **to the same session** (`--session`, not a new run — a new session
gets a fresh sandbox). Write a file first, wait for the suspension, then:

```bash
go run ./cmd/agent --session "<session-id-from-step-4>" \
  --prompt "Read /workspace/hello.txt"
```

The controller resumes the sandbox into a new pod that reattaches the
workspace PVC and reconnects the executor with the session's original
remote URL.

## Controller flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--namespace` | `$SANDBOX_NAMESPACE` or `default` | Namespace of the warm pool and claims |
| `--warmpool` | `oma-runner-pool` | SandboxWarmPool to claim from |
| `--kubeconfig`, `--context` | standard loading rules | Cluster access |
| `--poll-interval` | `5s` | Delay between session reconciliations |
| `--connect-timeout` | `4m` | Executor registration wait; must stay below the platform's 5-minute connection window |
| `--idle-suspend` | `30s` | Idle time before suspending a session sandbox |
| `--release-after` | `30m` | Idle time before deleting a session's claim |
| `--gc-interval` | `5s` | How often the idle sweep runs |
| `--runner-active-window` | `15m` | How long an unconfirmed turn keeps suspend/release off |
| `--runner-port` | `8080` | Runner port inside the sandbox |
| `--runner-grpc-port` | `9090` | Runner's idle SDK-tunnel placeholder port (must differ from `--runner-port`) |
| `--cleanup-on-exit` | `true` | On graceful shutdown, delete the claims of sessions the platform reports inactive |

The runner daemon (`cmd/runner`) takes `--exec-bin` (default
`$RUNNER_EXEC_BIN` or `codex`), `--exec-args` (default `exec-server`),
`--stop-grace` (default `10s`), and `--ready-pattern` (default
`(?i)^READY`).

## Notes

- **Session state lives in the workspace PVC.** Suspension terminates the
  pod; files under `/workspace` survive. Releasing the claim (after
  `--release-after`) deletes the PVC with it.
- **Any session in a sandbox can read `CODEX_API_KEY` from the pod
  environment.** Agent-generated code runs in the same pod as the
  executor, so inject only the restricted environment key there — never
  the application key — and do not reuse one environment across mutually
  untrusted sessions. The pod is the security boundary.
- **The runner's control endpoints are reachable from the cluster
  network.** Any pod that can route to a sandbox pod's IP can POST
  `/executor` or `/executor/stop`. The controller itself connects through
  apiserver port-forwards, which RBAC authorizes, so a NetworkPolicy that
  denies pod-to-pod ingress on the runner port does not break it — but
  enforcement requires a CNI that supports NetworkPolicy (kind's default
  kindnet ignores it). Apply one on multi-tenant clusters.
- The runner image holds an idle TCP listener on :9090 because the Go SDK
  requires its two tunnel ports to differ; the controller never speaks
  gRPC to the runner.
- The ready marker matches `fakeexec` and current codex output shapes, but
  stdout is not a documented contract. If a codex release stops printing a
  recognizable marker, the controller still confirms connections through
  the platform (the required action disappears); adjust `--ready-pattern`
  only to skip that cross-check path.
- A failed turn is not a bridge failure. The controller records the turn's
  terminal state and keeps the executor connected; investigate through the
  platform's session details.
- **The Agents API is in public beta.** The image pins
  `@openai/codex@0.173.0-alpha.1` (the self-hosted beta requires the alpha
  channel); record any bump in the `Dockerfile`. Per OpenAI's published
  limitations the API retains session state in the US and is not
  ZDR-eligible; self-hosting the sandbox does not change that.
- The example runs a single controller with in-memory session state. Claim
  names are deterministic per session, so a restarted controller adopts
  the existing sandbox when its session resurfaces; claims of sessions the
  platform reports inactive are cleaned up on graceful exit
  (`--cleanup-on-exit`), and any orphan manually with
  `kubectl delete sandboxclaim`.
- Unit tests need no cluster and no credentials: `go test -race ./...`.

## Cleanup

```bash
kubectl --context kind-agent-sandbox delete sandboxwarmpool oma-runner-pool
kubectl --context kind-agent-sandbox delete sandboxtemplate oma-runner-template
kubectl --context kind-agent-sandbox delete secret oai-env-key
```

## Acknowledgements

The executor is OpenAI's official
[`@openai/codex`](https://www.npmjs.com/package/@openai/codex) CLI, used
per OpenAI's
[self-hosted sandbox guidance](https://developers.openai.com/api/docs/guides/agents-api/environments/self-hosted).
The bridge reuses the architecture, lifecycle mechanics (reservation,
runner-active window, UID-based replacement readiness), and kind test
scaffolding of its sibling
[claude-managed-agents](../claude-managed-agents/) bridge.
