# Claude Code Sandbox (Headless, SDK-Driven)

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code) headlessly inside an Agent Sandbox and drive it entirely from the Python SDK: one-shot `-p` runs, JSON output parsing for `session_id`, and `--resume` for multi-turn conversations. A PVC-backed `~/.claude` keeps session state for the lifetime of the sandbox.

## What this demonstrates

- The **headless coding-agent pattern**: `claude -p "<prompt>" --output-format json` returns `result`, `session_id`, and usage in one JSON object; `claude -p ... --resume <session_id>` continues the same conversation. This is the building block for agents-orchestrating-agents.
- **Warm pool claims**: the demo sandbox comes from a `SandboxWarmPool`, so claim latency is seconds, not image-pull minutes.
- **SDK command execution**: everything runs through `sandbox.commands.run()` from [`k8s-agent-sandbox`](../../clients/python/agentic-sandbox-client/README.md) — no `kubectl exec`.
- **Secret-based credential injection**: the Anthropic API key never passes through the driver script; it is mounted into the pod from a Kubernetes Secret.

## Prerequisites

1. A Kubernetes cluster with the [Agent Sandbox controller and CRDs](../../README.md#installation) installed, **including the extension CRDs** (`SandboxTemplate` / `SandboxWarmPool` / `SandboxClaim` — e.g. `EXTENSIONS=true make deploy-kind`).
2. The [Sandbox Router](../../clients/python/agentic-sandbox-client/README.md#setup-deploying-the-router) deployed (the SDK's default Tunnel mode port-forwards to it).
3. The Python SDK: `pip install k8s-agent-sandbox`.
4. Docker, and [kind](https://kind.sigs.k8s.io/) if using the quick path.
5. An Anthropic API key (or a compatible endpoint via `ANTHROPIC_BASE_URL`).

## Files

| File | Purpose |
| --- | --- |
| `Dockerfile` | `claude-code-sandbox:v1` — python-runtime-sandbox base (SDK data plane on :8888) + Node 24 + `@anthropic-ai/claude-code@2.1.304` |
| `template.yaml` | `SandboxTemplate`: Secret-injected env, PVCs for `/workspace` (5Gi) and `~/.claude` (1Gi), `IS_SANDBOX=1` |
| `warmpool.yaml` | `SandboxWarmPool` `claude-code-pool` with 1 warm replica |
| `run_claude.py` | Three-stage demo: one-shot → seed memory → `--resume` (asserts the model recalls "42") |
| `run-test-kind.sh` | Build + kind load + apply + wait, one command |

## Quick start (kind)

```bash
# 1. Create the API key secret (once). Use --from-file so the key never
#    appears in shell history or process arguments. Optional proxy-endpoint
#    keys: --from-literal=ANTHROPIC_BASE_URL=https://... --from-literal=ANTHROPIC_MODEL=...
kubectl -n default create secret generic claude-code-api-keys \
  --from-file=ANTHROPIC_API_KEY=/path/to/anthropic-api-key

# 2. Build the image, load it into kind, apply manifests, wait for the pool
./run-test-kind.sh

# 3. Run the demo (SANDBOX_KUBE_CONTEXT pins the SDK to the same kind
#    cluster the script deployed to; omit it to use the current context)
SANDBOX_KUBE_CONTEXT=kind-agent-sandbox python3 run_claude.py
```

Expected tail of the output:

```text
=== stage 3: resume the session ===
[result] 42
SUCCESS: session resumed across headless invocations.
```

## Manual path (any cluster)

`template.yaml` references the node-local image `claude-code-sandbox:v1` with `imagePullPolicy: Never`. For clusters that cannot see your Docker daemon:

```bash
docker build -t REGISTRY/claude-code-sandbox:v1 examples/claude-code-sandbox/
docker push REGISTRY/claude-code-sandbox:v1
# Edit template.yaml: set image: REGISTRY/claude-code-sandbox:v1 and imagePullPolicy: IfNotPresent
kubectl apply -f template.yaml -f warmpool.yaml
kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 \
  sandboxwarmpool/claude-code-pool --timeout=600s
python3 run_claude.py
```

The manifests carry no `metadata.namespace`, so `kubectl` places them in the current context's namespace (or an explicit `-n`). If your resources live outside `default`, pass the same `-n <namespace>` to every `kubectl` command above and set `SANDBOX_NAMESPACE` (and optionally `SANDBOX_WARMPOOL_NAME`) for `run_claude.py` and `run-test-kind.sh`. Set `SANDBOX_KUBE_CONTEXT` for `run_claude.py` whenever the target cluster is not the current kubeconfig context — the SDK then pins both claim creation and the port-forward tunnel to that context.

## How the headless pattern works

- `claude -p "<prompt>"` runs one non-interactive turn and exits.
- `--output-format json` prints a single JSON object: `result` (the reply), `session_id` (conversation handle), and `usage` metadata.
- `--resume <session_id>` replays the stored conversation, so the next turn has full context. Session files live under `~/.claude`, which the `claude-state` PVC persists.
- `IS_SANDBOX=1` tells Claude Code it runs in a container and relaxes its sandbox-environment warning.
- The driver caps each turn with `command_timeout=120`. Keep it below the sandbox-router's `--proxy-timeout` (180s default), otherwise the router answers 502/504 instead of the SDK reporting a timeout.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| Warm pod `CreateContainerConfigError` | Secret `claude-code-api-keys` missing or key misspelled; create it, then delete this pool's warm sandboxes so they are recreated (`-n <namespace>` if not `default`): `kubectl -n <namespace> delete sandbox -l "$(kubectl -n <namespace> get sandboxwarmpool claude-code-pool -o jsonpath='{.status.selector}')"` |
| Stage fails with authentication error text in stdout | Invalid/expired `ANTHROPIC_API_KEY`, or proxy endpoint needs `ANTHROPIC_BASE_URL` in the Secret |
| `claude command timed out` | Raise `COMMAND_TIMEOUT` in `run_claude.py` (keep < router proxy timeout) or use a faster model via `ANTHROPIC_MODEL` |
| 502/504 from the router | Turn exceeded `--proxy-timeout`; see above |

## Acknowledgements

The headless JSON + session-resume flow is adapted from the [OpenSandbox `claude-code` example](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/claude-code) (Apache-2.0), rewritten against the Agent Sandbox CRDs and Python SDK.
