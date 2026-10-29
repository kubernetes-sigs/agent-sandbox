# Deep Agents on Agent Sandbox

Run a [Deep Agent](https://github.com/langchain-ai/deepagents) whose file and shell tools execute inside an Agent Sandbox pod. The [`deepagents-k8s-agent-sandbox`](../../clients/integrations/deepagents) integration adapts the `k8s-agent-sandbox` Python SDK to the Deep Agents `BaseSandbox` interface, so every `ls` / `read_file` / `write_file` / `glob` / `grep` / shell command the agent performs runs in the sandbox instead of on your machine.

The script claims a sandbox from a `SandboxWarmPool`, asks the agent to write and run a Fibonacci script, prints the agent's answer, and deletes the claim on exit. The sandbox is ephemeral: every file the agent creates is destroyed with the claim, so copy out anything you want to keep before the script exits.

## Security posture

The sandbox pod runs untrusted, agent-generated code. The template hardens it: no API credentials (`automountServiceAccountToken: false`), non-root uid 1000, all Linux capabilities dropped, and a read-only mount for the injected `sandboxd` binary.

The template omits `spec.networkPolicy`, so the controller applies its managed secure default: ingress is allowed only from the Sandbox Router (this script connects via `kubectl port-forward`, which bypasses pod networking), and egress is limited to the public internet — cluster-internal RFC1918 addresses and cloud metadata endpoints are blocked. For custom rules, see [`composing-sandbox-nw-policies`](../composing-sandbox-nw-policies/).

## Prerequisites

- A kind cluster with the agent-sandbox controller **and extension controllers**:

  ```bash
  EXTENSIONS=true make deploy-kind
  export KUBECONFIG=$PWD/bin/KUBECONFIG
  ```

- Python >= 3.11.
- An Anthropic API key (the default Deep Agents model):

  ```bash
  export ANTHROPIC_API_KEY=sk-ant-...
  ```

## Deploy the sandbox runtime

```bash
kubectl apply -f sandbox-template.yaml -f warmpool.yaml

# Wait until the warm pool reports one ready sandbox
kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 sandboxwarmpool/deepagents-warmpool -n default --timeout=300s
```

## Run the example

From this directory:

```bash
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
python main.py
```

The script prints the agent's final message, then verifies the work against the sandbox itself: it re-runs `fib.py` inside the pod and checks that the output contains the sequence `0, 1, 1, 2, 3, 5, 8, 13, 21, 34` (any print format). A successful run ends with:

```
Sandbox verification passed: fib.py runs in the pod and prints the expected sequence.
```

and exit code 0; a failed verification exits non-zero.

Verify that the claim was cleaned up:

```bash
kubectl get sandboxclaim -n "${AGENT_SANDBOX_NAMESPACE:-default}"
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_SANDBOX_NAMESPACE` | `default` | Namespace of the template and warm pool. |
| `AGENT_SANDBOX_WARMPOOL` | `deepagents-warmpool` | Warm pool the script claims from. |
| `ANTHROPIC_API_KEY` | _(required)_ | Anthropic API key for the default Deep Agents model. |
| `DEEPAGENTS_MODEL` | _(deepagents default)_ | Optional model string passed to `create_deep_agent`, e.g. `anthropic:claude-sonnet-4-5`. Set this when your API endpoint serves other models. |

## Troubleshooting

- **`sandboxwarmpool ... not found` or claim stuck pending**: the extension controllers are not running. Redeploy with `EXTENSIONS=true make deploy-kind`, then re-apply the manifests.
- **Cold start on first claim**: with `replicas: 1` the pool keeps one warm pod. If a previous run drained it, the next claim cold-starts (pod create + image pull); readiness is bounded by the SDK's `sandbox_ready_timeout` (180s default).
- **Anthropic authentication errors**: export a valid `ANTHROPIC_API_KEY` in the shell that runs `main.py`.
- **Leftover claims after a crash**: the script labels its claims `deepagents.example/claim=true`, so `kubectl delete sandboxclaim -n "${AGENT_SANDBOX_NAMESPACE:-default}" -l deepagents.example/claim=true` removes this example's claims from the namespace the script used, and nothing else. The warm pool refills automatically, and the claim's TTL (`shutdown_after_seconds`) deletes it server-side even without manual cleanup.

## Cleanup

```bash
kubectl delete -f warmpool.yaml -f sandbox-template.yaml
```

## References

- [Deep Agents](https://github.com/langchain-ai/deepagents) — the agent harness.
- [deepagents-k8s-agent-sandbox](../../clients/integrations/deepagents) — the Agent Sandbox backend for Deep Agents.
- [OpenSandbox deep-agents example](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/deep-agents) — the equivalent example on OpenSandbox that inspired this one.
