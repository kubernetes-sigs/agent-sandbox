# openai-agents-k8s-sandbox (prototype)

Runs OpenAI Agents SDK `SandboxAgent` workloads on
[Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox), as an
out-of-tree sandbox provider.

It ships as a separate package rather than as a PR to `openai-agents-python`, whose
maintainers
[do not currently accept in-tree third-party providers](https://github.com/openai/openai-agents-python/issues/3468).

Status: prototype. Exercised end-to-end against an in-process fake pod; **not yet run
against a live cluster** — see [Before trusting it](#before-trusting-it).

## Usage

The agent definition does not change; only the run config does.

```python
from agents import Runner
from agents.run import RunConfig
from agents.sandbox import SandboxRunConfig
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxInClusterConnectionConfig

from openai_agents_k8s_sandbox import K8sSandboxClient, K8sSandboxClientOptions

sandbox_client = AsyncSandboxClient(
    connection_config=SandboxInClusterConnectionConfig(),
    cleanup=False,  # see "Lifecycle ownership" below
)

run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=K8sSandboxClient(sandbox_client),
        options=K8sSandboxClientOptions(
            warm_pool="python-sandbox-pool",
            namespace="agents",
            shutdown_after_seconds=3600,
        ),
    ),
)

result = await Runner.run(agent, "summarize the repo", run_config=run_config)
```

## Cluster prerequisites

- Agent Sandbox controller + extensions installed (`sandbox-with-extensions.yaml`).
- A `SandboxTemplate` and a `SandboxWarmPool` whose image runs the in-pod sandbox server on
  port 8888 (e.g. `python-runtime-sandbox`). A stock `python:3.14-slim` will **not** work.
- The image needs a POSIX shell, `tar`, and `base64` on `PATH`, and a writable workspace root.
- For manifest `users`/`groups`: `sudo` in the image, because the SDK prefixes `sudo -u`
  itself when a tool runs as a non-default user.

## How it maps

| SDK contract | Implementation |
| --- | --- |
| `create` | `AsyncSandboxClient.create_sandbox(warm_pool, …)`, TTL via `shutdown_after_seconds` |
| `resume` | `get_sandbox(claim, warmpool_name=…)`; if the claim is gone, provision a replacement and let `start()` restore from `state.snapshot` |
| `delete` | `delete_sandbox(claim)` — deletes the `SandboxClaim`, cascading to pod and PVC |
| `_exec_internal` | `commands.run()`; argv is `shlex.join`-ed and the pod's shell reconstructs it |
| `read` / `write` | `files.read` / `files.write` (bytes-clean), or base64 over exec |
| `persist_workspace` | `tar` inside the pod to a staging file, then download it |
| `hydrate_workspace` | validate the tar locally, upload it, `tar -x` inside the pod |
| `running` | `status()[0] == "SandboxReady"` |
| `resolve_exposed_port` | the sandbox's stable in-cluster DNS name (`{sandbox}.{ns}.svc.cluster.local`) |
| PTY | not supported — the HTTP API has no PTY channel |

Mounts: no provider-specific mount strategy is needed. `InContainerMountStrategy` with
`RcloneMountPattern` already works if the image ships `rclone` (S3, GCS, R2, Azure Blob, Box).

## Design notes

**Transport seam.** Everything the session needs from the pod is three operations
(`exec_command`, `read_file`, `write_file`) behind `SandboxTransport`. Today's
implementation is HTTP; when the [`sandboxd` gRPC `ProcessService`](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/packages/sandboxd/spec)
lands (streaming, `WriteStdin`, `ResizeTTY`), it drops in without touching the session.

**Binary safety.** `ExecutionResult.stdout` is `str`, so the exec endpoint is lossy for
binary. File bytes and workspace tars never travel that path — they use the
upload/download endpoints. When exec is the only option (`file_transfer="exec"`, or a read
that must run as a specific user), the payload is base64-encoded.

**Absolute paths.** The in-pod client's own sanitizer strips a leading `/`, which would
silently relocate workspace writes. This provider passes `allow_unsafe_paths=True` and
sends absolute paths. A test asserts absolute paths actually reach the endpoints.

**Lifecycle ownership.** `AsyncSandboxClient` defaults to `cleanup=True`, registering an
`atexit` hook that deletes every tracked sandbox. That destroys sandboxes a later process
intends to `resume()`. Pass `cleanup=False` and rely on `shutdown_after_seconds`.

**`timeout=None`.** The SDK's "no timeout" becomes `exec_timeout_default_s` (300s), since
the HTTP API has no unbounded mode.

## Tests

```bash
.venv/bin/python -m pytest tests -q
```

`tests/fake_k8s.py` stands in for `k8s_agent_sandbox`'s async client and backs it with the
local machine — "the pod is this host". The real transport and session code run against
it, including the shell probes the SDK issues during `start()`, so the tests cover the
provider contract rather than mocks of it.

## Before trusting it

Unverified against a live cluster, in rough order of risk:

1. **Absolute paths on the server.** Does the in-pod server resolve `/workspace/x` as an
   absolute path, or relative to its own cwd? If the latter, set `file_transfer="exec"`.
2. **Concurrency.** The SDK issues parallel `exec` calls. Confirm the in-pod server handles
   concurrent `POST execute`.
3. **`tar --exclude`.** Assumes GNU-style `--exclude` before the member list. Busybox tar
   differs.
4. **Warm-pool cold start** under the SDK's `sandbox_ready_timeout`.
5. **Snapshot restore across pods** — the replacement-claim path in `resume()`.
