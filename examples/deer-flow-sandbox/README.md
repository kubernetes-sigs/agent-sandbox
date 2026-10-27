# DeerFlow on agent-sandbox

Run [DeerFlow](https://github.com/bytedance/deer-flow) agent turns whose shell
and file tools execute inside agent-sandbox `Sandbox` pods. This is a
configuration-level drop-in replacement for the
[OpenSandbox deer-flow example](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/deer-flow):
DeerFlow only changes its `sandbox.use` line; sandboxes become Kubernetes pods
claimed through a `SandboxWarmPool` instead of containers on a host Docker
daemon.

| | OpenSandbox backend | agent-sandbox backend (this example) |
| --- | --- | --- |
| Sandbox unit | Container on the host Docker daemon | `Sandbox` pod (via `SandboxClaim`) |
| Lifecycle API | OpenSandbox server (`domain`/`protocol`/`api_key`) | Kubernetes API + `k8s-agent-sandbox` SDK (`KUBECONFIG`) |
| Image | `sandbox.image` per config | `SandboxTemplate` (`sandbox-template.yaml`, stock `python:3.11` + injected [sandboxd](../../packages/sandboxd/)) |
| Warm pool | OpenSandbox warm pool | `SandboxWarmPool` (`warmpool.yaml`) |
| Tool transport | OpenSandbox SDK | sandboxd: gRPC ProcessService (commands) + REST filesystem (files), reached by pod port-forward |
| `ready_timeout` | server-side sandbox readiness | seconds to wait for the claim to bind |
| `sandbox_timeout` | sandbox TTL | claim TTL (`shutdownTime`, auto-delete); cached sandboxes are checked against TTL deletion on `acquire()` |
| `bash_command_timeout` | server-side command timeout | per-command gRPC deadline via sandboxd (default 600s) |
| `idle_timeout` | server-side reaper | not enforced; `release()` destroys the claim |

## Files

- [agent_sandbox_provider/provider.py](./agent_sandbox_provider/provider.py) —
  `AgentSandboxProvider` (DeerFlow `SandboxProvider`) and `AgentSandbox`
  (DeerFlow `Sandbox` tool surface) built on the
  [`k8s-agent-sandbox`](../../clients/python/agentic-sandbox-client/) SDK.
- [agent_sandbox_provider/parsing.py](./agent_sandbox_provider/parsing.py) +
  [test_parsing.py](./test_parsing.py) — cluster-free parsing helpers
  (path translation, line windows, `find`/`grep` output) and their unit tests.
- [sandbox-template.yaml](./sandbox-template.yaml) — `SandboxTemplate`:
  `python:3.11` with the sandboxd binary injected by an initContainer
  (topology B from [examples/sandboxd-sandbox](../sandboxd-sandbox/)); no
  custom image build required.
- [warmpool.yaml](./warmpool.yaml) — `SandboxWarmPool` backing the SDK's
  claim-based acquire. Required: the SDK creates claims against a warm pool;
  claims that arrive on an empty pool cold-start within `ready_timeout`.
- [main.py](./main.py) — model-free script that drives the provider directly
  and asserts the full tool surface.

## Prerequisites

1. A cluster with the agent-sandbox controller **and extension controllers**
   (the example uses `SandboxTemplate`/`SandboxWarmPool`/`SandboxClaim`):

   ```bash
   EXTENSIONS=true make deploy-kind
   export KUBECONFIG=$PWD/bin/KUBECONFIG
   ```

2. Apply the manifests:

   ```bash
   kubectl apply -f examples/deer-flow-sandbox/sandbox-template.yaml
   kubectl apply -f examples/deer-flow-sandbox/warmpool.yaml
   ```

   Both manifests pin `namespace: default` (the convention across
   `examples/`), matching the provider's default. If you set
   `AGENT_SANDBOX_NAMESPACE`, edit the `namespace` in **both** files to
   match before applying: the provider creates its claims in that namespace
   and will not find a template or warm pool living in another one.

3. A [deer-flow](https://github.com/bytedance/deer-flow) checkout with the
   harness installed (not published on PyPI), plus this project's Python SDK
   with the gRPC extra:

   ```bash
   git clone https://github.com/bytedance/deer-flow.git
   cd deer-flow/backend && uv sync --all-packages
   uv pip install "k8s-agent-sandbox[grpc]"
   ```

## Run the model-free demo

From this directory (no model or API key needed):

```bash
uv run --project /path/to/deer-flow/backend python main.py
```

It acquires a sandbox, exercises `execute_command`, `write_file`, `read_file`,
`list_dir`, `glob`, `grep`, and `download_file` with output assertions, then
destroys the claim. Optional env: `AGENT_SANDBOX_NAMESPACE` (default
`default`), `AGENT_SANDBOX_WARMPOOL` (default `deer-flow-warmpool`).

## Run a real DeerFlow agent turn

Save the config below to a file (e.g. `deer-flow-sandbox.yaml` in this
example's directory) and select it with `DEER_FLOW_CONFIG_PATH` — otherwise
DeerFlow loads its own default config and never reaches
`AgentSandboxProvider`. Put this example's directory on `PYTHONPATH` so
`agent_sandbox_provider` is importable, and run the CLI through deer-flow's
uv environment (`uv sync` does not put `deerflow` on the shell's `PATH`):

```yaml
sandbox:
  use: agent_sandbox_provider.provider:AgentSandboxProvider
  image: python:3.11
  ready_timeout: 300
  sandbox_timeout: 600
  idle_timeout: 0
```

```bash
cd /path/to/this/examples/deer-flow-sandbox
PYTHONPATH=$PWD DEER_FLOW_CONFIG_PATH=$PWD/deer-flow-sandbox.yaml \
  uv run --project /path/to/deer-flow/backend \
  deerflow --print "Run python3 -c 'print(2 ** 16)' and report the output"
```

## Notes

- The sandboxd binary comes from the staging image
  `us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:latest-main`
  (same dependency as [examples/sandboxd-sandbox](../sandboxd-sandbox/)).
- `release()` deletes the `SandboxClaim` via the SDK's idempotent
  `Sandbox.terminate()` (destroy semantics; a failed teardown still falls
  back to a server-side claim delete and raises if that too fails, so no
  caller sees success on a leaked claim); the warm pool replenishes so the
  next `acquire()` is fast.
- Sandboxes run untrusted agent code: non-root, all capabilities dropped, no
  service-account token. Add a `NetworkPolicy` (see
  [examples/composing-sandbox-nw-policies](../composing-sandbox-nw-policies/))
  if you need egress control.
- The workspace `emptyDir` is mounted at sandboxd's root itself
  (`/mnt/user-data`), so the root and its `workspace`/`uploads`/`outputs`
  subdirectories are all writable by the sandbox user (`fsGroup: 1000`).
  The volume is still ephemeral: data does not survive claim/pod deletion.
- `SandboxClaim` names derive from the harness user+thread scope token
  (`derive_sandbox_scope_token(user_id, thread_id)`), so two users never
  share a sandbox even if they share a thread id; an `acquire()` without a
  `thread_id` always mints a fresh sandbox.
- Commands run under a per-command ceiling, `sandbox.bash_command_timeout`
  (default 600s): exceeding it yields output ending in the harness's
  `Exit Code: 124` marker. A per-call `timeout=0` means "no limit" (same
  sentinel as `idle_timeout: 0`), and `execute_command` renders failures as
  text (`... Exit Code: N`, `Error: ...`) per the DeerFlow contract rather
  than raising.
- Per-Agent **skill projection is not supported** by this provider: with an
  explicit Agent skill policy the harness fails closed before acquiring
  (`supports_agent_skill_isolation` is False, as for the OpenSandbox
  reference backend), and unrestricted runs use the image's own tooling.
  The pod's non-root user cannot project files into the skills root outside
  sandboxd's `--root-dir`.
