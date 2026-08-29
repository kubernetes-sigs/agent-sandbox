# OpenHands × agent-sandbox: `AgentSandboxWorkspace`

A workspace backend for the [OpenHands agent SDK](https://github.com/OpenHands/software-agent-sdk)
that runs conversations on **pre-warmed agent-sandbox pods** instead of
cold-starting a Docker container per session.

OpenHands' `DockerWorkspace` pays image pull + container start + agent-server
boot on every workspace. `AgentSandboxWorkspace` replaces that with a
**warm-pool claim — a bind, not a boot**: the pod is already Running, the
agent-server already healthy (the template's readinessProbe guarantees it).
Everything after provisioning is inherited from the SDK's `RemoteWorkspace`,
which speaks HTTP to the agent-server: bash, file transfer, git operations —
this package adds no execution plumbing of its own. You also get agent-sandbox's
isolation posture (gVisor runtime class, network policy, per-pod resource
limits) for the model-driven code the agent executes.

## Quickstart

1. Install the controller + extensions ([releases](https://github.com/kubernetes-sigs/agent-sandbox/releases)),
   then create the template and pool (edit the image tag to match your
   `openhands-sdk` version — they are released together):

```bash
kubectl create secret generic openhands-session-key --from-literal=key="$(openssl rand -hex 24)"
kubectl apply -f configs/agent-server-template.yaml
kubectl apply -f configs/agent-server-warmpool.yaml
```

2. Install and use:

```bash
pip install openhands-k8s-agent-sandbox
```

```python
from openhands_k8s_agent_sandbox import AgentSandboxWorkspace

with AgentSandboxWorkspace(
    warmpool="agent-server-pool",
    namespace="default",
    api_key="<value of the openhands-session-key secret>",
    ttl_s=3600,                       # controller-side backstop cleanup
) as workspace:
    result = workspace.execute_command("echo hello from a warm pod")
    print(result.stdout)
```

The workspace works anywhere OpenHands accepts a workspace object —
`Conversation(agent=..., workspace=workspace)` runs the whole agent loop
against the warm pod.

## How it maps onto `DockerWorkspace`

| `DockerWorkspace` | `AgentSandboxWorkspace` |
|---|---|
| `docker run` agent-server image, port-map | claim from `SandboxWarmPool` (pod already Running) |
| wait up to 120 s for `/health` | 10 s budget — Ready pods are healthy by construction; fail fast and re-claim |
| `host = http://127.0.0.1:{port}` | `host = http://{pod_ip}:8000` (VPC-routable), or `endpoint_template` for gateway/proxied paths |
| `docker stop` on exit | delete the claim (pool replenishes); `ttl_s` backstops dead clients |

## Notes

- **Auth is pool-level.** Pre-warmed servers cannot take a per-claim key, so all
  pods in a pool share `OH_SESSION_API_KEYS_0` from one Secret; pass the same
  value as `api_key`. Scope pools/namespaces accordingly.
- **Version skew.** The SDK pins its agent-server image per release; keep the
  template image tag aligned with your installed `openhands-sdk`
  (`workspace.get_server_info()` shows the server's version).
- **Pods are single-conversation.** Claims are deleted on close, not reused —
  reuse across conversations needs a reset story (see the sandbox-recycling
  design notes before attempting it).
