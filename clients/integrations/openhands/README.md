# OpenHands × Agent Sandbox: `AgentSandboxWorkspace`

A workspace backend for the [OpenHands agent SDK](https://github.com/OpenHands/software-agent-sdk)
that runs conversations on **pre-warmed Agent Sandbox pods** instead of cold-starting
a Docker container per session.

OpenHands' `DockerWorkspace` pays image pull + container start + agent-server boot on
every workspace. `AgentSandboxWorkspace` replaces that with a **warm-pool claim — a
bind, not a boot**: the pod is already Running and the agent-server already healthy
(the template's readinessProbe guarantees it). Everything after provisioning is
inherited from the SDK's `RemoteWorkspace`, which speaks HTTP to the agent-server —
bash, file transfer, git operations; this package adds no execution plumbing of its
own. You also get Agent Sandbox's isolation posture (gVisor runtime class, network
policy, per-pod resource limits) for the model-driven code the agent executes.

| `DockerWorkspace` | `AgentSandboxWorkspace` |
|---|---|
| `docker run` agent-server image, port-map | claim from a `SandboxWarmPool` (pod already Running) |
| wait up to 120 s for `/health` | 10 s budget — Ready pods are healthy by construction; fail fast and re-claim |
| `host = http://127.0.0.1:{port}` | `host = http://{pod_ip}:8000`, or `endpoint_template` for gateway/proxied paths |
| `docker stop` on exit | delete the claim (pool replenishes); `ttl_s` backstops dead clients |

## Prerequisites

- **A Kubernetes cluster** with the Agent Sandbox controller **and extensions**
  installed (`Sandbox`, `SandboxClaim`, `SandboxTemplate`, `SandboxWarmPool` CRDs):

  ```bash
  VERSION=v1.0.0   # pin to a release
  kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox-with-extensions.yaml
  ```

- **`kubectl` + a kube context** — the client reads your kubeconfig.
- **Network reachability from the client to the pods.** The default endpoint is the
  pod IP on port 8000. On GKE, pod IPs are VPC-routable, so anything in the VPC
  (GCE VMs, other clusters) works out of the box; from outside the VPC use
  `endpoint_template` to route through a gateway/proxy instead.
- **Python ≥ 3.12** (the `openhands-sdk` floor).
- *(Recommended)* a **gVisor node pool** and the `gvisor` RuntimeClass — agents run
  model-generated code; see the repo's gVisor quickstart.

## Deploy

1. **Session key** (pool-level auth — see [Auth model](#auth-model)):

   ```bash
   # -n must match the namespace the template/pool manifests use (default);
   # the pods read the secret from their own namespace.
   kubectl create secret generic openhands-session-key -n default \
     --from-literal=key="$(openssl rand -hex 24)"
   ```

2. **SandboxTemplate** — defines what an agent-server pod is. Edit the image tag in
   [`configs/agent-server-template.yaml`](./configs/agent-server-template.yaml) to
   match your installed `openhands-sdk` version (SDK and agent-server image are
   released together), then:

   ```bash
   kubectl apply -f configs/agent-server-template.yaml
   ```

   The template's readinessProbe on `/health:8000` is **load-bearing**: it is what
   makes a Ready pod equal a healthy agent-server, which is what lets the client use
   a short health budget.

3. **SandboxWarmPool** — how many pods sit Ready. `replicas` should be at least your
   peak *concurrent* conversations:

   ```bash
   kubectl apply -f configs/agent-server-warmpool.yaml
   ```

4. **Verify** the pool is filled:

   ```bash
   kubectl get sandboxwarmpool agent-server-pool
   kubectl get pods -l 'agents.x-k8s.io/sandbox-template=openhands-agent-server'
   ```

## Install

```bash
pip install openhands-k8s-agent-sandbox
# for the agent half of the example (tool presets):
pip install openhands-tools
```

## Quickstart

Workspace operations only (no LLM needed):

```python
from openhands_k8s_agent_sandbox import AgentSandboxWorkspace

with AgentSandboxWorkspace(
    warmpool="agent-server-pool",
    namespace="default",
    api_key="<value of the openhands-session-key secret>",
    ttl_s=3600,
) as workspace:
    print(workspace.get_server_info())          # server version — check skew
    result = workspace.execute_command("echo hello from a warm pod && uname -a")
    print(result.stdout)
```

A full agent conversation — the standard SDK pattern with exactly one class swapped
relative to the SDK's own `DockerWorkspace` examples:

```python
import os
from pydantic import SecretStr
from openhands.sdk import LLM, Conversation
from openhands.tools.preset.default import get_default_agent
from openhands_k8s_agent_sandbox import AgentSandboxWorkspace

llm = LLM(usage_id="agent", model=os.environ["LLM_MODEL"],
          api_key=SecretStr(os.environ["LLM_API_KEY"]))

with AgentSandboxWorkspace(warmpool="agent-server-pool",
                           api_key=os.environ["SANDBOX_SESSION_KEY"]) as workspace:
    agent = get_default_agent(llm=llm, cli_mode=True)
    conversation = Conversation(agent=agent, workspace=workspace)
    conversation.send_message("Write 3 facts about this environment into FACTS.txt")
    conversation.run()
    conversation.close()
```

Or run the ready-made [`example.py`](./example.py):

```bash
export SANDBOX_WARMPOOL=agent-server-pool SANDBOX_SESSION_KEY=<secret value>
export LLM_API_KEY=... LLM_MODEL=...        # optional: enables the agent phase
python example.py
```

## Configuration reference

| field | default | meaning |
|---|---|---|
| `warmpool` | *(required)* | `SandboxWarmPool` to claim from |
| `namespace` | `default` | namespace of the pool |
| `api_key` | `None` | sent as `X-Session-API-Key`; must equal the pool's session key |
| `working_dir` | `/workspace` | agent/tool cwd inside the pod |
| `server_port` | `8000` | agent-server port inside the pod |
| `endpoint_template` | `None` | URL template for gateway/proxied data paths; supports `{pod_ip}`, `{port}`, `{namespace}`, `{claim_name}`, `{sandbox_id}`; mutually exclusive with `router_url` |
| `router_url` | `None` | base URL of a [sandbox-router](../../python/agentic-sandbox-client/sandbox-router) deployment; all traffic (health check included) goes to the router with `X-Sandbox-*` routing headers injected |
| `router_auth_token` | `None` | Bearer token for the router (`ROUTER_AUTH_TOKEN`); stripped by the router before forwarding, so it composes with `api_key` |
| `claim_timeout_s` | `60` | wait for the claim to bind a Ready sandbox |
| `health_check_timeout` | `10.0` | wait for `/health`; deliberately short — a warm pod that isn't healthy is broken, fail fast and re-claim |
| `ttl_s` | `None` | claim TTL (`spec.lifecycle` shutdownTime) — the controller deletes the claim on expiry even if this process died |
| `claim_labels` | `None` | labels on the `SandboxClaim` object |
| `sandbox_client` | `None` | inject a configured `k8s_agent_sandbox.SandboxClient`; built with defaults when omitted |

## Routing through sandbox-router

When pod IPs aren't routable from the client (off-VPC clients, laptop dev), route
through the client SDK's [sandbox-router](../../python/agentic-sandbox-client/sandbox-router)
instead of raw pod IPs:

```python
AgentSandboxWorkspace(
    warmpool="agent-server-pool",
    router_url="http://sandbox-router.default.svc:8080",   # or its LB/Gateway address
    router_auth_token="<ROUTER_AUTH_TOKEN>",
    api_key="<pool session key>",
)
```

The workspace sends `X-Sandbox-ID`/`-Namespace`/`-Port`/`-Pod-IP` routing headers on
every request (health check included); paths pass through the router verbatim, so
the agent-server protocol is unchanged. Two auth layers compose: the Bearer token
authenticates to the router, which strips `Authorization` before forwarding, while
`X-Session-API-Key` passes through to the agent-server. The router's proxy timeout
(default 180 s) is compatible with the SDK's start-then-poll command pattern — no
long-lived requests. Laptop tip: the router is a regular runc Deployment, so
`kubectl port-forward svc/sandbox-router` works even when the sandboxes themselves
are gVisor pods (where direct pod port-forward does not — see Troubleshooting).

## Auth model

Pre-warmed servers cannot receive a per-claim key, so **all pods in a pool share**
the session key the template injects (`OH_SESSION_API_KEYS_0` from one Secret); pass
the same value as `api_key`. Scope pools and namespaces accordingly, and treat key
rotation as a pool rollout (update the Secret, then recreate the pool so pods pick it
up). Leaving key and `api_key` unset is acceptable only on a private, trusted network.

## Lifecycle semantics

- A claim **binds** an existing warm pod; nothing boots on the critical path.
- `close()` / context-manager exit / GC **deletes the claim**; the pool replenishes
  in the background. Pods are **single-conversation** — reuse across conversations
  needs a reset story (see the sandbox-recycling design notes before attempting it).
- Set `ttl_s` in anything long-running: it is the server-side backstop that reaps
  claims from clients that died without cleanup.

## Sizing and scale

- Pool `replicas` ≈ peak concurrent conversations, plus slack for replenish lag.
- For large pools (hundreds+), raise the controller's worker counts
  (`--sandbox-concurrent-workers`, `--sandbox-claim-concurrent-workers`,
  `--sandbox-warm-pool-concurrent-workers`) — see `docs/configuration.md`.
- **Per-task images at high cardinality** (e.g. SWE-bench-style pools per repo
  image) and rolling warm windows are the territory of
  [`examples/agent-sandbox-rl`](../../../examples/agent-sandbox-rl) — use its fleet
  as the off-cluster pre-warmer and claim here by per-image pool name.

## Troubleshooting

| symptom | likely cause / fix |
|---|---|
| claim times out | pool empty or still filling — check `kubectl get sandboxwarmpool`; raise `replicas` or `claim_timeout_s` |
| health check fails instantly | probe path/port mismatch in the template, or the pod isn't the agent-server image — a Ready pod must mean a serving `/health:8000` |
| `no pod IP` / connect timeouts | client can't route to pod IPs — use `endpoint_template` through a gateway, or run the client inside the VPC |
| `kubectl port-forward` refused while the pod is Ready | expected with gVisor: the forwarder dials loopback in the host-side netns, which isn't connected to runsc's netstack. Pod-IP traffic is unaffected; for laptop dev use a runc template variant or an in-VPC runner |
| HTTP 401 from the server | `api_key` doesn't match the pool's `OH_SESSION_API_KEYS_0` Secret value |
| odd protocol errors | SDK ↔ server version skew — compare `workspace.get_server_info()` with your installed `openhands-sdk` version and realign the template image tag |

## Development

```bash
pip install -e ../../python/agentic-sandbox-client -e '.[test]'
python -m pytest tests/unit -q
```
