# Claude Managed Agents on Agent Sandbox

Run [Claude Managed Agents (CMA) self-hosted sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
work — tool calls such as bash and file operations — inside `Sandbox` pods.
A local controller polls the CMA work API, claims one warm sandbox per agent
session, and suspends idle sandboxes; an in-sandbox runner daemon executes
the tool calls and heartbeats the work lease.

This is one bridge of the [managed-agents](../../managed-agents/README.md) example collection;
that README explains how these bridges map onto Agent Sandbox in general.
This one adapts [agent-substrate/claude-managed-agents](https://github.com/agent-substrate/claude-managed-agents)
(Apache-2.0) from Agent Substrate actors to the Agent Sandbox CRDs and Go
SDK. The upstream Valkey event queue is replaced by in-process per-session
serialization: once a work item is acknowledged and leased, the queue only
provided ordering, which a single-process controller can do itself.

## CMA work protocol

CMA exposes self-hosted environments as a leased work queue. The contract
this example implements end to end:

1. `cmd/agent` creates a CMA agent (`agent_toolset_20260401`, `always_allow`
   policy for the demo) and opens a session on your environment with an
   initial prompt.
2. The controller's poller claims the resulting work item: **poll** (with
   `reclaim_older_than_ms=5000`) → **ack** → **first heartbeat** with
   `expected_last_heartbeat=NO_HEARTBEAT` and a hand-off TTL (default 120s).
   The response's `last_heartbeat` is the lease token.
3. The session manager claims a warm sandbox (`SandboxClaim` against
   `cma-runner-pool`), port-forwards to the runner on :8080, and POSTs
   `{session_id, work_id, last_heartbeat}` to `/work`.
4. The runner's environment worker **continues the lease** — its first
   heartbeat quotes the handed-off token — executes the tool calls with the
   Anthropic SDK's agent toolset, and streams results back to CMA. The
   controller polls `GET /status` until the turn finishes.
5. After 30s idle (configurable) the controller suspends the sandbox. The
   pod terminates but the workspace PVC survives; the next work item resumes
   the sandbox into a **new pod** that reattaches the volume. After 30m idle
   the claim is deleted entirely.

Failure semantics follow the lease: an item whose ack fails is left for CMA
to re-deliver; an item that is acked but cannot be heartbeated is
force-stopped; a runner whose first heartbeat gets `412 Precondition Failed`
(the item was re-claimed elsewhere) exits without touching it. A crashed
controller is recovered by the hand-off TTL: leases lapse and CMA
re-delivers the items to a later poll.

## Prerequisites

1. A Kubernetes cluster with the Agent Sandbox controller **and the extension
   CRDs/controllers** installed (`EXTENSIONS=true make deploy-kind` for kind).
2. Anthropic credentials for CMA self-hosted sandboxes:
   `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, and
   `ANTHROPIC_ENVIRONMENT_KEY` — see
   [Creating a Self-Hosted Environment](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes#create-an-environment).
3. Go 1.26+, Docker, and [kind](https://kind.sigs.k8s.io/) for the quick path.
4. Cluster egress from sandbox pods to `api.anthropic.com` (the runner calls
   the Anthropic API directly).

## Quick start

The commands below target the kind cluster `run-test-kind.sh` deploys to. If
your current kubeconfig context is different, add
`--context kind-${KIND_CLUSTER_NAME:-agent-sandbox}` to every `kubectl`
command and `--context kind-${KIND_CLUSTER_NAME:-agent-sandbox}` to
`go run ./cmd/controller`.

### 1. Create the credentials Secret

```bash
kubectl --context kind-agent-sandbox -n "${SANDBOX_NAMESPACE:-default}" \
  create secret generic cma-credentials \
  --from-file=ANTHROPIC_ENVIRONMENT_ID=/path/to/environment-id \
  --from-file=ANTHROPIC_ENVIRONMENT_KEY=/path/to/environment-key
```

### 2. Build and deploy the warm pool

```bash
./run-test-kind.sh
```

The script builds `cma-runner:v1` (build context: the repo root), loads it
into the kind cluster named by `KIND_CLUSTER_NAME` (default `agent-sandbox`),
applies `template.yaml` and `warmpool.yaml`, and waits for one warm sandbox.

On a non-kind cluster, push the image to a registry and change `image:` in
`template.yaml` (keeping `imagePullPolicy: IfNotPresent` or switching to
`Always`).

### 3. Start the controller

```bash
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."
go run ./cmd/controller --context kind-agent-sandbox
```

### 4. Start a session

In another terminal, from this directory — the credentials must be exported
in this terminal too:

```bash
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."
go run ./cmd/agent --prompt "Run 'uname -a' in bash and show the output."
```

The agent command prints the session ID and a `platform.claude.com` session
URL. Watch the controller log the claim, the work item, and — after the turn
finishes and 30s pass — the suspension:

```bash
kubectl --context kind-agent-sandbox get sandbox -w
```

To see the resume path reuse the session's workspace, send a follow-up
prompt **to the same session** (`--session`, not `--agent` — a new session
gets a fresh sandbox). Write a file first, wait for the suspension, then:

```bash
go run ./cmd/agent --session "<session-id-from-step-4>" \
  --prompt "Read /workspace/hello.txt"
```

The controller resumes the sandbox into a new pod that reattaches the
workspace PVC.

## Controller flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--namespace` | `$SANDBOX_NAMESPACE` or `default` | Namespace of the warm pool and claims |
| `--warmpool` | `cma-runner-pool` | SandboxWarmPool to claim from |
| `--kubeconfig`, `--context` | standard loading rules | Cluster access |
| `--environment-id` | `$ANTHROPIC_ENVIRONMENT_ID` | CMA self-hosted environment |
| `--anthropic-base-url` | `$ANTHROPIC_BASE_URL` | Custom Anthropic API URL |
| `--idle-suspend` | `30s` | Idle time before suspending a session sandbox |
| `--release-after` | `30m` | Idle time before deleting a session's claim |
| `--handoff-ttl` | `120` | Lease seconds the poller's first heartbeat requests |
| `--poll-interval` | `1s` | Delay between polls when no work is available |
| `--runner-port` | `8080` | Runner port inside the sandbox |
| `--runner-grpc-port` | `9090` | Runner's idle SDK-tunnel placeholder port (must differ from `--runner-port`) |
| `--gc-interval` | `5s` | How often the idle sweep runs |
| `--cleanup-on-exit` | `true` | Delete this controller's claims on graceful shutdown |

## Notes

- **Session state lives in the workspace PVC.** Suspension terminates the
  pod; files under `/workspace` survive. Releasing the claim (after
  `--release-after`) deletes the PVC with it.
- The runner's file tools are confined to `/workspace` and `/tmp`; the pod
  is the security boundary.
- **Any session on your environment can obtain `ANTHROPIC_ENVIRONMENT_KEY`.**
  The bash tool runs as the same UID as the runner, so a model-issued command
  can read the key from `/proc/1/environ` even though the SDK scrubs it from
  the shell environment. The key is environment-wide: do not share one CMA
  environment across mutually untrusted sessions.
- The runner image holds an idle TCP listener on :9090 because the Go SDK
  requires its two tunnel ports to differ; the controller never speaks gRPC
  to the runner.
- The example runs a single controller with in-memory session state. A
  controller restart loses the session→sandbox mapping; orphaned claims are
  cleaned up on graceful exit (`--cleanup-on-exit`) or manually with
  `kubectl delete sandboxclaim`.

## Cleanup

```bash
kubectl --context kind-agent-sandbox delete sandboxwarmpool cma-runner-pool
kubectl --context kind-agent-sandbox delete sandboxtemplate cma-runner-template
kubectl --context kind-agent-sandbox delete secret cma-credentials
```

## Acknowledgements

Adapted from [agent-substrate/claude-managed-agents](https://github.com/agent-substrate/claude-managed-agents)
(Apache-2.0, Copyright 2026 Google LLC), which runs the same CMA self-hosted
workload on Agent Substrate actors. See its `DESIGN.md` for the lease
hand-off protocol this example preserves.
