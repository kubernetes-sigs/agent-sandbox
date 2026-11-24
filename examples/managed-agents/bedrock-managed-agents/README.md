# Bedrock Managed Agents on Agent Sandbox

Run [Amazon Bedrock Managed Agents (BMA), powered by OpenAI](https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-managed-agents-openai.html)
work — the bash, file, and local-MCP tool calls of a session driven by
the hosted harness — inside `Sandbox` pods. A local controller polls BMA
for managed sessions, claims one warm sandbox per session, attaches the
session's executor (`codex exec-server`) through the in-sandbox runner
daemon, and suspends idle sandboxes; the executor connects out to BMA
directly with AWS SigV4 and streams tool results back without passing
through the controller.

This is one bridge of the [managed-agents](../../managed-agents/README.md) example
collection; that README explains how these bridges map onto Agent Sandbox
in general. BMA offers two execution-environment types: `aws_bedrock_agentcore`
(an AWS-managed AgentCore Runtime) and `self_hosted` (your own compute).
This bridge implements the `self_hosted` side with Kubernetes sandboxes —
the AgentCore Runtime option runs its compute inside AWS and has nothing
for Agent Sandbox to provide. Unlike the CMA bridge, the platform ships
its own executor binary, so the runner supervises `codex exec-server`
instead of executing tool calls in-process. The protocol resembles the
OpenAI Agents API (same session/event/item shapes under
`/openai/v1/...`), with two differences that shape this bridge: requests
are signed with AWS SigV4 instead of API keys, and the preview publishes
no connection signal — the controller keeps the executor attached and
reads turn state from the session status.

## BMA work protocol

The contract this example implements end to end:

1. `cmd/agent` creates a session with `environment.type = "self_hosted"`,
   workspace `/workspace`, and the marker
   `[managed-by: agent-sandbox-example]` appended to the agent
   instructions (BMA has no metadata field; the controller only ever
   touches sessions carrying the marker), then sends the initial prompt.
2. The controller's reconciler lists managed sessions and re-reads each
   one before acting — the list can be stale, and the marker check must
   run against current state.
3. The session manager claims a warm sandbox (`SandboxClaim` against
   `bma-runner-pool`, with a deterministic claim name derived from the
   session ID so a restarted controller adopts the existing claim),
   port-forwards to the runner on :8080, and POSTs `{session_id,
   environment_id, attempt_id}` to `/executor`. The runner starts
   `codex exec-server --remote <endpoint>/openai/v1 --environment-id <id>
   --remote-transport direct --aws-sigv4 --aws-service bedrock-mantle
   --aws-region <region>`; the executor signs registration and its
   WebSocket with the pod's AWS credentials.
4. The controller waits for the executor's ready marker, then polls the
   session status until the turn settles (`in_progress` → `idle`). BMA's
   preview surface has no turn-list API; `cmd/agent` reads the turn's
   durable items for the result.
5. After 30s idle (configurable) — and only if BMA reports no turn in
   flight — the controller stops the executor gracefully and suspends the
   sandbox. The pod terminates but the workspace PVC survives; the next
   turn resumes the sandbox into a **new pod** that reattaches the volume,
   and the executor reattaches **with the same environment ID**. After 30m
   idle the claim is deleted entirely (a later turn then starts with an
   empty workspace).

Failure semantics follow the attachment rather than a lease: a start
request whose response is lost (transport error, 5xx) is *uncertain* — the
executor may be starting, so lifecycle cleanup stays off for a bounded
window. A runner `409` means the sandbox is bound to a different session;
because that binding is long-lived, the controller abandons and re-claims
instead of retrying. Two failed attach attempts put the session in a
terminal `executor_failed` state (GC still releases its sandbox on
schedule). A crashed executor is restarted by the controller's periodic
attach recheck (every minute by default): BMA, unlike the OpenAI Agents
API, re-publishes nothing when the connection drops. Reconnection never
replays timed-out input, so the controller never re-sends prompts; it
reads turn outcomes from the session status. A 404 on a managed session
(the application deleted it) releases the sandbox immediately, because
deleting a BMA session does not reclaim self-hosted compute.

## Prerequisites

1. A Kubernetes cluster with the Agent Sandbox controller **and the
   extension CRDs/controllers** installed (`EXTENSIONS=true make
   deploy-kind` for kind).
2. An AWS account with access to the BMA public preview, in a supported
   region (`us-east-1`, `us-east-2`, or `us-west-2`).
3. The IAM roles from AWS's official
   [example bundle](https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-managed-agents-openai-self-hosted.html):
   its `self-hosted` CDK stack creates the session role
   (`BedrockManagedAgentsPreviewInferenceServiceRole`) that BMA assumes,
   and a client role for API calls. This repo does not duplicate that
   stack.
4. Credentials, split by trust level:
   - **Controller-side identity** (AWS profile or environment) for
     `cmd/controller` and `cmd/agent`: BMA API permissions plus
     `iam:PassRole` for the session role. It must never reach a sandbox.
   - **Pod-side identity** for the sandbox pods, injected as the
     `bma-env-key` Secret (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
     optionally `AWS_SESSION_TOKEN`). The preview does not publish a
     minimal policy; use a restricted identity scoped to BMA and rotate it
     if a sandbox ran untrusted code.
5. Go 1.26+, Docker, and [kind](https://kind.sigs.k8s.io/) for the quick
   path.
6. Cluster egress from sandbox pods to
   `https://bedrock-mantle.<region>.api.aws` (HTTPS and WebSocket; the
   executor connects out directly).

## Quick start

The commands below target the kind cluster `run-test-kind.sh` deploys to.
If your current kubeconfig context is different, add `--context
kind-${KIND_CLUSTER_NAME:-agent-sandbox}` to every `kubectl` and `go run
./cmd/controller` command.

### 1. Create the credentials Secret

```bash
kubectl --context "kind-${KIND_CLUSTER_NAME:-agent-sandbox}" -n "${SANDBOX_NAMESPACE:-default}" \
  create secret generic bma-env-key \
  --from-literal=AWS_ACCESS_KEY_ID=<pod-side-access-key-id> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<pod-side-secret-access-key> \
  --dry-run=client -o yaml | \
kubectl --context "kind-${KIND_CLUSTER_NAME:-agent-sandbox}" -n "${SANDBOX_NAMESPACE:-default}" apply -f -
```

Add `--from-literal=AWS_SESSION_TOKEN=<token>` for temporary credentials.
If your pod-side region differs from the `us-east-1` default, edit
`AWS_REGION` and `BMA_ENDPOINT` in `template.yaml` to match. Pods pick the
new values up when they are recreated; step 2 recycles the warm sandboxes.

### 2. Build and deploy the warm pool

```bash
./run-test-kind.sh
```

The script builds `bma-runner:v1` (build context: the repo root), loads it
into the kind cluster named by `KIND_CLUSTER_NAME` (default
`agent-sandbox`), applies `template.yaml` and `warmpool.yaml`, and waits
for one warm sandbox. Without the Secret it creates placeholder
credentials and warns — enough for the credential-free link-level test
below, not for real sessions.

On a non-kind cluster, push the image to a registry and change `image:` in
`template.yaml` (keeping `imagePullPolicy: IfNotPresent` or switching to
`Always`).

With `E2E=true`, the script points the template at `fakeexec` — a
same-flag stand-in for the executor baked into the image — and drives the
full Kubernetes-side lifecycle without AWS credentials: warm claim, runner
protocol (start/duplicate/conflict), suspend with pod termination, resume
into a new pod (UID-checked), workspace PVC retention, and release:

```bash
E2E=true ./run-test-kind.sh
```

The e2e run leaves `bma-runner-template` patched to `fakeexec`. Before
starting the controller for real sessions, run `./run-test-kind.sh` again
without `E2E=true`: it re-applies `template.yaml` (restoring the real
executor configuration) and recreates the warm sandboxes from it.

### 3. Start the controller

```bash
export AWS_PROFILE=my-bma-client-profile
go run ./cmd/controller --context kind-agent-sandbox --region us-east-1
```

### 4. Start a session

In another terminal, from this directory — the same AWS profile must be
available here too:

```bash
export AWS_PROFILE=my-bma-client-profile
export BMA_INFERENCE_ROLE_ARN=arn:aws:iam::123456789012:role/BedrockManagedAgentsPreviewInferenceServiceRole
go run ./cmd/agent --region us-east-1 \
  --prompt "Run 'uname -a' in bash and show the output."
```

The agent command prints the session and environment IDs, waits for the
turn to settle, and prints the turn's items. Watch the controller log the
claim, the executor attach, and — after the turn settles and 30s pass —
the suspension:

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
workspace PVC and reattaches the executor with the session's original
environment ID.

## Controller flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--namespace` | `$SANDBOX_NAMESPACE` or `default` | Namespace of the warm pool and claims |
| `--warmpool` | `bma-runner-pool` | SandboxWarmPool to claim from |
| `--region` | `$BMA_REGION`, `$AWS_REGION`, or `us-east-1` | BMA signing region |
| `--endpoint` | `$BMA_ENDPOINT` or `https://bedrock-mantle.<region>.api.aws` | BMA endpoint override |
| `--kubeconfig`, `--context` | standard loading rules | Cluster access |
| `--poll-interval` | `5s` | Delay between session reconciliations |
| `--connect-timeout` | `4m` | How long to wait for the executor's ready marker |
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
`(?i)^READY`). It reads `BMA_ENDPOINT`, `BMA_SERVICE` (default
`bedrock-mantle`), and `AWS_REGION` from the pod environment.

## Notes

- **Session state lives in the workspace PVC.** Suspension terminates the
  pod; files under `/workspace` survive. Releasing the claim (after
  `--release-after`) deletes the PVC with it.
- **Any session in a sandbox can read the pod's AWS credentials from its
  environment.** Agent-generated code runs in the same pod as the
  executor, so inject only the restricted pod-side identity there — never
  the controller-side one — and do not reuse one environment across
  mutually untrusted sessions. The pod is the security boundary.
- **Network posture of the runner endpoints.** The shipped
  `template.yaml` leaves `networkPolicyManagement` at its default
  (`Managed`) with no custom `networkPolicy`, so the controller applies
  its secure-default policy: ingress is allowed only from the Sandbox
  Router, which blocks pod-to-pod calls to `/executor` or
  `/executor/stop`. The endpoints are only open when the template sets
  `Unmanaged`, when a custom `networkPolicy` allows wider ingress, or on
  a CNI that does not enforce NetworkPolicy (kind's default kindnet).
  The controller connects through apiserver port-forwards, which RBAC
  authorizes. As a second layer, the runner refuses to start an executor
  against a `BMA_ENDPOINT` that is not an HTTPS `api.aws` host, so a
  misconfigured controller cannot point the pod's credentials elsewhere.
- The runner image holds an idle TCP listener on :9090 because the Go SDK
  requires its two tunnel ports to differ; the controller never speaks
  gRPC to the runner.
- The ready marker matches `fakeexec` and current codex output shapes, but
  stdout is not a documented contract. BMA exposes no connection state to
  cross-check, so if a codex release stops printing a recognizable marker,
  point `--ready-pattern` at the line its log prints when the exec-server
  connection is established (`RUST_LOG=codex_exec_server=debug` makes it
  visible).
- A failed turn is not a bridge failure. The controller records the
  session's `failed` state and stops dispatching it; GC releases the
  sandbox on schedule. Inspect the turn through the BMA items API.
- **BMA is in public preview.** The preview API can change; the image pins
  `@openai/codex@0.172.0` (BMA requires ≥ 0.154.0, the first line with the
  exec-server AWS SigV4 flags) — record any bump in the `Dockerfile`.
- The example runs a single controller with in-memory session state. Claim
  names are deterministic per session, so a restarted controller adopts
  the existing sandbox when its session resurfaces; claims of sessions the
  platform reports inactive are cleaned up on graceful exit
  (`--cleanup-on-exit`), and any orphan manually with
  `kubectl delete sandboxclaim`.
- Unit tests need no cluster and no credentials: `go test -race ./...`.

## Cleanup

Stop the controller first (Ctrl+C): `--cleanup-on-exit` releases the
claims of sessions the platform reports inactive. Delete any session
claims that remain (names start with `bma-`), then the pool resources:

```bash
kubectl --context kind-agent-sandbox -n "${SANDBOX_NAMESPACE:-default}" get sandboxclaim
kubectl --context kind-agent-sandbox -n "${SANDBOX_NAMESPACE:-default}" delete sandboxclaim <leftover-claim>
kubectl --context kind-agent-sandbox -n "${SANDBOX_NAMESPACE:-default}" delete sandboxwarmpool bma-runner-pool
kubectl --context kind-agent-sandbox -n "${SANDBOX_NAMESPACE:-default}" delete sandboxtemplate bma-runner-template
kubectl --context kind-agent-sandbox -n "${SANDBOX_NAMESPACE:-default}" delete secret bma-env-key
```

Remove the IAM stack through AWS's example bundle cleanup flow; keep it
if another session still uses its roles.

## Acknowledgements

The executor is OpenAI's official
[`@openai/codex`](https://www.npmjs.com/package/@openai/codex) CLI, used
per AWS's
[self-hosted compute guidance](https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-managed-agents-openai-self-hosted.html)
for BMA. The bridge reuses the architecture, lifecycle mechanics
(reservation, runner-active window, UID-based replacement readiness), and
kind test scaffolding of its sibling
[openai-managed-agents](../openai-managed-agents/) and
[claude-managed-agents](../claude-managed-agents/) bridges.
