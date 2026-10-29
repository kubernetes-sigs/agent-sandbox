# OpenCode in an Agent Sandbox

Run [OpenCode](https://opencode.ai/), an open-source AI coding agent, inside a `Sandbox` pod. The example bakes the official `opencode-ai` CLI into a container image and runs prompts non-interactively with `opencode run`. Two paths share the same image:

1. **kubectl path** — a plain `Sandbox`; drive it with `kubectl exec` (or attach to the OpenCode TUI). Needs only the core controller.
2. **SDK path** — a `SandboxTemplate` + `SandboxWarmPool`; drive it from Python with the [`k8s-agent-sandbox`](../../clients/python/agentic-sandbox-client/README.md) SDK. Needs the extension controllers and the sandbox-router.

By default OpenCode uses its free [OpenCode Zen](https://opencode.ai/docs/zen/) model, so no API key is required.

> **Warning:** The free model is offered for a limited time. Do not send private code, credentials, or other confidential data to it; review the provider's data-use terms first. Set `OPENCODE_MODEL` (in the `opencode-api-keys` Secret, or in the driver's environment for the SDK path) to use an authenticated model instead.

## Prerequisites

1. A Kubernetes cluster (kind, Minikube, GKE, …) with `kubectl` configured.
2. The [Agent Sandbox controller and CRDs](../../README.md#installation) installed. The SDK path additionally requires the **extension CRDs** (e.g. `EXTENSIONS=true make deploy-kind`) and the [sandbox-router](../../clients/python/agentic-sandbox-client/README.md#setup-deploying-the-router).
3. Docker (to build the image); [kind](https://kind.sigs.k8s.io/) for the quick path.
4. SDK path only: Python ≥ 3.11 with `pip install k8s-agent-sandbox`.
5. Optional: an LLM API key (Anthropic, or an OpenCode Zen key) for authenticated models.

## Files

| File | Purpose |
| --- | --- |
| `Dockerfile` | `opencode-sandbox:v1` — python-runtime-sandbox base (SDK data plane on :8888) + Node 24 + `opencode-ai@1.19.5` |
| `sandbox.yaml` | Path 1: plain `Sandbox` with `stdin`/`tty`, workspace and state PVCs, optional Secret env |
| `template.yaml` | Path 2: `SandboxTemplate` with `:8888` probes and optional Secret env |
| `warmpool.yaml` | Path 2: `SandboxWarmPool` `opencode-pool` with 1 warm replica |
| `run_opencode.py` | Path 2: claims a warm sandbox and runs one `opencode run` turn via the SDK |
| `run-test-kind.sh` | Path 2: build + kind load + apply + wait, one command |

## Optional: create the API key Secret

Skip this to use the free default model. Otherwise create the Secret once (use `--from-file` so keys stay out of shell history; include only the keys you need):

```bash
kubectl -n default create secret generic opencode-api-keys \
  --from-file=ANTHROPIC_API_KEY=/path/to/anthropic-api-key
# Optional OpenCode Zen overrides:
#   --from-literal=OPENCODE_API_KEY=... --from-literal=OPENCODE_MODEL=anthropic/claude-...
```

## Path 1: kubectl

Build and load the image, then apply the Sandbox:

```bash
docker build -t opencode-sandbox:v1 .
kind load docker-image opencode-sandbox:v1 --name "${KIND_CLUSTER_NAME:-agent-sandbox}"  # kind only
kubectl apply -f sandbox.yaml
kubectl wait --for=condition=Ready pod/opencode-sandbox --timeout=300s
```

Run a prompt non-interactively:

```bash
kubectl exec sandbox/opencode-sandbox -- \
  opencode run "Compute 1+1 and reply with only the final number."
```

Or attach to the OpenCode TUI (detach with `Ctrl+P`, `Ctrl+Q`):

```bash
kubectl attach -it pod/opencode-sandbox
```

**Non-kind clusters:** push `opencode-sandbox:v1` to a registry you control, then edit `image:` in `sandbox.yaml` (and `template.yaml` for path 2) before applying.

## Path 2: Python SDK (warm pool)

```bash
# Build, load into kind, apply template + warmpool, wait for the warm pod
./run-test-kind.sh

# Claim a warm sandbox and run one opencode turn
SANDBOX_KUBE_CONTEXT=kind-agent-sandbox python3 run_opencode.py
```

Expected tail of the output:

```text
[stdout] ... 2 ...
SUCCESS: opencode run completed inside the sandbox.
```

`run_opencode.py` honors `SANDBOX_NAMESPACE` (default `default`), `SANDBOX_WARMPOOL_NAME` (default `opencode-pool`), `SANDBOX_KUBE_CONTEXT` (unset uses the current context), and `OPENCODE_MODEL` (default `opencode/deepseek-v4-flash-free`, passed as `opencode run --model`).

## Cleanup

```bash
# Path 1
kubectl delete -f sandbox.yaml
kubectl delete pvc -l sandbox=opencode-sandbox --ignore-not-found

# Path 2 (run_opencode.py terminates its own claim; clean up stragglers by name)
kubectl delete -f warmpool.yaml -f template.yaml
kubectl get sandboxclaim                 # list any remaining claims
# kubectl delete sandboxclaim <name>     # delete stragglers individually

# Optional Secret
kubectl delete secret opencode-api-keys --ignore-not-found
```

## Configuration

- **Different provider/model:** put `OPENCODE_MODEL` (and the matching API key) into the `opencode-api-keys` Secret, or export `OPENCODE_MODEL` for `run_opencode.py`. See [OpenCode Providers](https://opencode.ai/docs/providers) for per-provider environment variables.
- **PVC sizes:** edit `volumeClaimTemplates` in `sandbox.yaml` / `template.yaml`.
- **Newer OpenCode:** bump the version in `Dockerfile` (`opencode-ai@<new-version>`) and rebuild.

## References

- [OpenCode CLI](https://opencode.ai/docs/cli/) — installation and the non-interactive `run` command
- [OpenCode Providers](https://opencode.ai/docs/providers) — provider authentication and configuration
- [OpenCode Zen](https://opencode.ai/docs/zen/) — model availability and data-use notes

## Acknowledgements

Adapted from the [OpenSandbox `opencode` example](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/opencode) (Apache-2.0), rewritten against the Agent Sandbox CRDs and Python SDK.
