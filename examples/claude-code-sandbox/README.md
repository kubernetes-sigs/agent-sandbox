# Claude Code Sandbox (Headless, SDK-Driven)

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code) headlessly inside an Agent Sandbox and drive it entirely from the Python SDK: one-shot `-p` runs, JSON output parsing for `session_id`, and `--resume` for multi-turn conversations. A PVC-backed `~/.claude` keeps session state for the lifetime of the sandbox.

## What this demonstrates

- The **headless coding-agent pattern**: `claude -p "<prompt>" --output-format json` returns `result`, `session_id`, and usage in one JSON object; `claude -p ... --resume <session_id>` continues the same conversation. This is the building block for agents-orchestrating-agents.
- **Warm pool claims**: the demo sandbox comes from a `SandboxWarmPool`, so claim latency is seconds, not image-pull minutes.
- **Suspend and resume**: the driver suspends the sandbox between turns, checks that the controller replaces the Pod, then continues the conversation from PVC-backed state.
- **SDK command execution**: everything runs through `sandbox.commands.run()` from [`k8s-agent-sandbox`](../../clients/python/agentic-sandbox-client/README.md) — no `kubectl exec`.
- **Secret-based credential injection**: the Anthropic API key never passes through the driver script; it is mounted into the pod from a Kubernetes Secret.

## Prerequisites

1. A Kubernetes cluster with the [Agent Sandbox controller and CRDs](../../README.md#installation) installed, **including the extension CRDs** (`SandboxTemplate` / `SandboxWarmPool` / `SandboxClaim` — e.g. `EXTENSIONS=true make deploy-kind`).
2. The [Sandbox Router](../../clients/python/agentic-sandbox-client/README.md#setup-deploying-the-router) deployed (the SDK's default Tunnel mode port-forwards to it).
3. The Python SDK installed from this Git checkout. From the repository root,
   run `pip install -e clients/python/agentic-sandbox-client`. The example needs
   `suspend()` and `resume()`, which are not available in the PyPI release 1.0.11.
   After installation, run `cd examples/claude-code-sandbox`.
4. Docker, and [kind](https://kind.sigs.k8s.io/) if using the quick path.
5. An Anthropic API key (or a compatible endpoint via `ANTHROPIC_BASE_URL`).

## Files

| File | Purpose |
| --- | --- |
| `Dockerfile` | `claude-code-sandbox:v1` — python-runtime-sandbox base (SDK data plane on :8888) + Node 24 + `@anthropic-ai/claude-code@2.1.304` |
| `template.yaml` | `SandboxTemplate`: Secret-injected env, PVCs for `/workspace` (5Gi) and `~/.claude` (1Gi), `IS_SANDBOX=1` |
| `warmpool.yaml` | `SandboxWarmPool` `claude-code-pool` with 1 warm replica |
| `run_claude.py` | One-shot → seed memory → suspend/resume the sandbox → `--resume` (asserts the model recalls "42" on a replacement Pod) |
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
=== stage 3: suspend and resume the sandbox ===
[sandbox] Pod replaced.

=== stage 4: resume the session ===
[result] 42

SUCCESS: session resumed after sandbox suspend and resume.
```

## Manual path (any cluster)

`template.yaml` references the node-local image `claude-code-sandbox:v1` with `imagePullPolicy: Never`. For clusters that cannot see your Docker daemon:

```bash
docker build -t REGISTRY/claude-code-sandbox:v1 .
docker push REGISTRY/claude-code-sandbox:v1
# Edit template.yaml: set image: REGISTRY/claude-code-sandbox:v1 and imagePullPolicy: IfNotPresent
kubectl apply -f template.yaml -f warmpool.yaml
kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 \
  sandboxwarmpool/claude-code-pool --timeout=600s
python3 run_claude.py
```

The manifests carry no `metadata.namespace`, so `kubectl` places them in the current context's namespace (or an explicit `-n`). If your resources live outside `default`, pass the same `-n <namespace>` to every `kubectl` command above and set `SANDBOX_NAMESPACE` (and optionally `SANDBOX_WARMPOOL_NAME`) for `run_claude.py` and `run-test-kind.sh`. Set `SANDBOX_KUBE_CONTEXT` for `run_claude.py` whenever the target cluster is not the current kubeconfig context — the SDK then pins both claim creation and the port-forward tunnel to that context.

## Keep the provider key outside the sandbox

Use an Anthropic-compatible model proxy to keep the provider credential outside
this sandbox. The proxy holds that credential. The sandbox still needs a
restricted caller token that the proxy accepts through `ANTHROPIC_API_KEY`.
This does not make the sandbox credential-free.

For a new deployment, use this **instead of** the quick-start Secret command.
The example below assumes a proxy Service named `model-proxy` in namespace
`model-proxy`, with Service port 8080 and Pods labeled `app: model-proxy`
listening on TCP 8080. Use the Service port in `ANTHROPIC_BASE_URL` and the Pod's
listening port in the network policy if they differ.
Use the proxy's ClusterIP instead of its DNS name so the sandbox does not need
DNS access. Allowing cluster DNS would let the agent discover in-cluster Service
names. Use your proxy's supported model name. For a different target cluster,
replace `kind-agent-sandbox` in both commands with its kubeconfig context.

```bash
PROXY_IP=$(kubectl --context kind-agent-sandbox -n model-proxy get service model-proxy -o jsonpath='{.spec.clusterIP}')
case "$PROXY_IP" in
  *:*) PROXY_IP="[$PROXY_IP]" ;;
esac
kubectl --context kind-agent-sandbox -n <namespace> create secret generic claude-code-api-keys \
  --from-file=ANTHROPIC_API_KEY=/path/to/proxy-caller-token \
  --from-literal=ANTHROPIC_BASE_URL="http://${PROXY_IP}:8080" \
  --from-literal=ANTHROPIC_MODEL=MODEL_NAME
```

If you recreate the proxy Service, read its ClusterIP again and update
`ANTHROPIC_BASE_URL` in the Secret before recreating the warm sandboxes.

Without an explicit `networkPolicy`, the controller uses public DNS resolvers
and blocks egress to private address ranges. This prevents reaching an in-cluster
proxy in those ranges. Setting `ANTHROPIC_BASE_URL` alone does not grant network
access.

Add the following `spec.networkPolicy` to `template.yaml` before creating the
warm pool. Adjust the namespace and Pod selectors to match your proxy Pods.
The proxy port must match the Pod's listening port, which may differ from
the Service port. Use a network plugin that enforces Kubernetes NetworkPolicy.

```yaml
spec:
  networkPolicy:
    ingress:
    - from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: agent-sandbox-system
        podSelector:
          matchLabels:
            app: sandbox-router
      ports:
      - protocol: TCP
        port: 8888
    egress:
    - to:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: model-proxy
        podSelector:
          matchLabels:
            app: model-proxy
      ports:
      - protocol: TCP
        port: 8080
```

This policy replaces the default ingress and egress rules. It keeps router
access and allows outbound connections only to the selected proxy Pods, without
allowing cluster DNS or direct HTTPS connections to the public provider. Check
other policies selecting these Pods: NetworkPolicies are additive, so another
policy can allow broader egress. Use HTTPS for the proxy endpoint if your
deployment requires encrypted in-cluster traffic, with a certificate valid for
the IP address.

If the pool already exists, apply the edited template and recreate its unclaimed
warm sandboxes with the pool-selector command in [Troubleshooting](#troubleshooting).
On kind, rerunning `run-test-kind.sh` with the same namespace and cluster settings
does both steps.

The shared policy changes immediately and denies all DNS egress, including to
public resolvers. Use fresh claims for this example. Do not apply this policy to
a template with active workloads that still need DNS access.

## How the headless pattern works

- `claude -p "<prompt>"` runs one non-interactive turn and exits.
- `--output-format json` prints a single JSON object: `result` (the reply), `session_id` (conversation handle), and `usage` metadata.
- `--resume <session_id>` replays the stored conversation, so the next turn has full context. Session files live under `~/.claude`, which the `claude-state` PVC persists.
- `sandbox.suspend()` deletes the Pod while retaining its PVCs. `sandbox.resume()` waits for the replacement Pod to become ready. The same SDK handle reconnects on the next command. The driver checks Pod UIDs because the replacement can have the same name.
- The resume check succeeds with the example's `~/.claude` mount. The separate `~/.claude.json` file is not on that PVC and does not survive Pod replacement. It is not required for this API-key-based headless resume flow.
- `IS_SANDBOX=1` tells Claude Code it runs in a container and relaxes its sandbox-environment warning.
- The driver caps each turn with `command_timeout=120`. Keep it below the sandbox-router's `--proxy-timeout` (180s default), otherwise the router answers 502/504 instead of the SDK reporting a timeout.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| Warm pod `CreateContainerConfigError` | Secret `claude-code-api-keys` missing or key misspelled; create it, then delete this pool's warm sandboxes so they are recreated (`-n <namespace>` if not `default`): `kubectl -n <namespace> delete sandbox -l "$(kubectl -n <namespace> get sandboxwarmpool claude-code-pool -o jsonpath='{.status.selector}')"` |
| Stage fails with authentication error text in stdout | Invalid/expired `ANTHROPIC_API_KEY`, or proxy endpoint needs `ANTHROPIC_BASE_URL` in the Secret |
| In-cluster proxy connection times out or its name does not resolve | Use the proxy's current ClusterIP, add the explicit proxy policy above, and recreate the warm sandboxes. Check the proxy Pod selector and listening port. A base URL alone does not override the default policy. |
| `claude command timed out` | Raise `COMMAND_TIMEOUT` in `run_claude.py` (keep < router proxy timeout) or use a faster model via `ANTHROPIC_MODEL` |
| 502/504 from the router | Turn exceeded `--proxy-timeout`; see above |

## Acknowledgements

The headless JSON + session-resume flow is adapted from the [OpenSandbox `claude-code` example](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/claude-code) (Apache-2.0), rewritten against the Agent Sandbox CRDs and Python SDK.
