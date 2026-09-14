# sandboxd User Guide

`sandboxd` is the portable sandbox runtime daemon defined by
[KEP-539.2](../../docs/keps/539.2-runtime-standardization/README.md). It runs
inside a sandbox pod and exposes the hybrid runtime API:

```text
sandboxd (runtime daemon)
├── gRPC  :9090  →  ProcessService    (streaming process I/O)
└── HTTP  :8080  →  FilesystemService (stateless file operations & runtime probes)
```

- **Process execution** is served over **gRPC** because `Start` is a
  long-lived server-streaming RPC: `stdout`/`stderr` flow continuously until
  the process exits.
- **Filesystem transfers** are served over **REST** so file bytes move as raw
  `application/octet-stream` payloads with no protobuf wrapping, and any
  plain HTTP client works without generated stubs.

Both listeners bind to `--listen-host` (default `0.0.0.0`), so the daemon is
reachable through the Pod IP, a Kubernetes Service, or an SDK pod
port-forward. The current `sandbox-router` accepts HTTP/2 client connections,
but disables HTTP/2 on its upstream connections and therefore cannot carry the
gRPC `ProcessService`, so it is not a complete `sandboxd` transport.
Containment is provided by pod isolation and NetworkPolicy, not loopback
binding; pass `--listen-host=127.0.0.1` to restrict to loopback (e.g. local
development).

**Where commands execute:** `ProcessService` runs commands inside whichever
container hosts the `sandboxd` process, using that container's root
filesystem. A shared volume (`/workspace`) shares *files* between containers —
it does **not** share binaries. This is the single most important fact for
choosing a [deployment topology](#deployment-topologies).

The specifications live in [`spec/`](spec/):

| Surface | Spec |
|---|---|
| `ProcessService` (gRPC) | [`spec/process/v1/process.proto`](spec/process/v1/process.proto) |
| Filesystem & Runtime REST API | [`spec/filesystem/v1/filesystem.yaml`](spec/filesystem/v1/filesystem.yaml) |

## Implementing a compatible runtime

`sandboxd` is the reference implementation, but SDKs depend on the wire
contracts above rather than on the `sandboxd` binary itself. An alternative
runtime is compatible when it provides both versioned surfaces against the
same workspace and execution environment:

- Serve `process.v1.ProcessService` without changing protobuf field numbers
  or RPC names. For a successfully started process, `Start` emits one
  `InitEvent` before process output, zero or more `stdout`/`stderr` events,
  and one final `ExitEvent`. The process ID from `InitEvent` is the handle
  used by `WriteStdin`, `SendSignal`, and `ResizeTTY`.
- Serve the `/v1/files`, `/v1/health`, and `/v1/metadata` REST resources with
  the status codes and JSON shapes in the OpenAPI document. File payloads are
  raw bytes rather than base64; `PUT` accepts both an octet-stream body and a
  multipart `file` part.
- Resolve filesystem paths and process working directories beneath one
  sandbox root, including symlink-aware traversal protection. A file written
  through REST must be visible to a process launched through gRPC.
- Preserve the documented readiness, metadata filtering, error, and
  concurrency semantics. Use standard gRPC status codes for RPC failures and
  the OpenAPI `Error` shape for REST failures.
- Treat `process.v1` and `/v1` as protocol versions. Backward-incompatible
  wire changes require a new version instead of silently changing the v1
  contract.

The runtime protocol is independent of the transport used to reach a sandbox.
A compatible proxy or router needs to route the REST and gRPC connections to
the same sandbox, preserve HTTP/2 and gRPC trailers, and stream file bodies and
`Start` responses without buffering them to completion. The Go SDK uses a
direct pod port-forward by default and also supports in-cluster Pod IP or
headless Service connectivity. The synchronous Python SDK uses a direct pod
port-forward.

A useful smoke test is to run the SDK filesystem and command operations against
the alternative implementation, followed by the reference integration scenario
in [`examples/sandboxd-sandbox/`](../../examples/sandboxd-sandbox/). This does
not replace a formal conformance suite; implementations should also verify every
RPC and REST operation and the documented error and concurrency semantics.

## Endpoint addressing

Workload-local applications and custom clients may use environment variables
such as the following to locate a co-located `sandboxd` instance:

```bash
SANDBOXD_GRPC_ADDR=localhost:9090
SANDBOXD_REST_ADDR=localhost:8080
```

These names are an application convention: `sandboxd` does not require them,
and the supported SDKs do not inspect them or automatically switch between
`sandboxd` and the legacy `python-runtime`. Select the runtime and connectivity
explicitly as shown in [Agent Sandbox SDK access](#agent-sandbox-sdk-access).

## API summary

### ProcessService (gRPC, `:9090`)

| RPC | Type | Purpose |
|---|---|---|
| `Start` | Server stream | Run a command, stream `stdout`/`stderr` in real time until `ExitEvent`. Optional PTY. |
| `Execute` | Unary | Run a command synchronously, return `stdout`/`stderr`/`exit_code` atomically. |
| `WriteStdin` | Unary | Send `stdin` bytes or `EOF` to a running process. |
| `SendSignal` | Unary | Deliver `SIGINT`/`SIGTERM`/`SIGKILL` to the process group. |
| `ResizeTTY` | Unary | Resize the pseudo-terminal window (`cols`, `rows`). |

Errors surface as standard gRPC status codes (`NOT_FOUND` for unknown
process IDs, `PERMISSION_DENIED` for a `cwd` escaping the sandbox root,
`FAILED_PRECONDITION` for `ResizeTTY` on a process without a PTY).

### Filesystem & Runtime REST API (`:8080`)

| Method | Endpoint | Purpose |
|---|---|---|
| `GET` | `/v1/files/{path}` | File → raw bytes (`application/octet-stream`); directory → JSON `DirectoryListing`. |
| `HEAD` | `/v1/files/{path}` | Existence/metadata probe without transferring the body. |
| `PUT` | `/v1/files/{path}` | Atomic write (temp file + rename), auto-creates parents. Optional `mode` query (`^0[0-7]{3}$`, default `0644`). Accepts raw bytes or `multipart/form-data` (`file` part). |
| `DELETE` | `/v1/files/{path}` | Remove a file or directory; `recursive=true` for `rm -rf` behavior; `409` on a non-empty directory otherwise. |
| `GET` | `/v1/health` | Readiness probe: `200 {"status":"ok"}` or `503` during shutdown. |
| `GET` | `/v1/metadata` | Orchestrator-injected, non-sensitive environment variables (allowlisted by prefix, default `SANDBOX_`). |

All `{path}` values are resolved against the sandbox root (`/workspace` by
default) via symlink-aware sanitization; traversal attempts return
`403 {"code":"PERMISSION_DENIED"}`.

### Concurrency semantics

Requests are handled in parallel, with no server-side serialization. The
atomic-write strategy prevents partial file contents, but it does not impose an
ordering across requests:

- **Write vs. read** — a successful read sees either the complete previous
  file or the complete new one, never partial content. A read racing with a
  rename or delete may instead fail.
- **Write vs. delete** — either operation may fail depending on ordering. For
  example, a recursive delete can remove a writer's temporary file or parent
  directory before the final rename.
- **Delete vs. read** — a download that has already opened the file can
  complete after the path is deleted; a read that has not opened it may fail.
- **Concurrent writes to one path** — each successful rename is atomic; the
  last successful rename determines the final contents.

There is no cross-request transaction or compare-and-swap; agents that need
ordering or coordination on shared paths must layer it themselves.

## Deployment topologies

The SDK and API are identical regardless of how `sandboxd` is deployed — but
**which container runs the `sandboxd` process decides where your commands
execute and which binaries they can see.** Pick the topology from that:

| | A — dedicated runtime container | B — inject into an existing image |
|---|---|---|
| Commands run in | sandboxd's own container rootfs | your app image's rootfs |
| Tools available | what's baked into the runtime image | everything already in your image |
| Needs image rebuild | yes (to add tools) | no (initContainer injects the binary) |
| Isolation | stronger (dedicated, minimal runtime) | weaker (daemon runs in your full image) |
| Use when | you can build the execution image | you cannot rebuild your image |

**A (default):** `sandboxd` runs as the pod's runtime container. Commands
execute in that container, so tools must be baked into its image (the base
image is minimal — a shell + coreutils; build `FROM sandboxd` to add `npm`,
`python3`, …). Strongest isolation: untrusted agent code runs in a
controlled runtime image.

**B (no-rebuild alternative):** an initContainer copies the statically-linked
`sandboxd` binary into a shared `emptyDir`, and your existing, unmodified
tool-rich image runs it as its command. Commands now execute inside *your*
image, so e.g. `npm install` finds `npm` — no rebuild required.

Complete, runnable manifests for both (plus an end-to-end SDK client) live in
[`examples/sandboxd-sandbox/`](../../examples/sandboxd-sandbox/).

> **A common pitfall:** running `sandboxd` as a *sidecar* — a separate
> container next to an (unmodified) workload container, sharing `/workspace`.
> This looks like topology B but behaves like topology A: the shared volume
> shares files, **not** the workload's binaries, so commands exec'd through
> `ProcessService` still run in the sandboxd container and cannot see the
> workload image's tools. A co-located workload container is useful only as a
> *client* of the sandboxd API (reaching it over pod-local networking), not
> as a source of tools.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--listen-host` | `0.0.0.0` | Interface for both listeners. Default is reachable on the pod network; set `127.0.0.1` to restrict to loopback. |
| `--grpc-port` | `9090` | Port for the gRPC ProcessService. |
| `--rest-port` | `8080` | Port for the REST API. |
| `--root-dir` | `/workspace` | Sandbox root confining all file operations and working directories. Created if missing. |
| `--metadata-env-prefix` | `SANDBOX_` | Env var prefix exposed on `/v1/metadata`. |
| `--shutdown-timeout` | `10s` | Grace period for in-flight requests and child processes. |
| `--http-idle-timeout` | `60s` | Close idle HTTP keep-alive connections after this duration. |
| `--stream-chunk-size` | `4096` | Buffer size in bytes for streaming process stdout/stderr chunks. |
| `--version` | | Print version info and exit. |

## Talking to sandboxd

### curl (REST filesystem)

```bash
# Write a file (atomic, parents auto-created)
curl -sf -X PUT -H "Content-Type: application/octet-stream" \
  --data-binary @local.py "localhost:8080/v1/files/src/main.py?mode=0644"

# Read it back
curl -sf localhost:8080/v1/files/src/main.py

# List a directory (JSON)
curl -sf localhost:8080/v1/files/src

# Existence probe
curl -sf -I localhost:8080/v1/files/src/main.py

# Delete recursively
curl -sf -X DELETE "localhost:8080/v1/files/src?recursive=true"

# Probes
curl -sf localhost:8080/v1/health
curl -sf localhost:8080/v1/metadata
```

### grpcurl (ProcessService)

```bash
# Synchronous execution
grpcurl -plaintext -d '{"config":{"command":["echo","hello"]}}' \
  localhost:9090 process.v1.ProcessService/Execute

# Streaming execution: watch InitEvent → stdout chunks → ExitEvent
grpcurl -plaintext -d '{"config":{"command":["sh","-c","for i in 1 2 3; do echo $i; sleep 1; done"]}}' \
  localhost:9090 process.v1.ProcessService/Start

# PTY execution: stty and tty only succeed with a real terminal attached,
# so this verifies genuine PTY allocation (expect "24 80" and /dev/pts/N)
grpcurl -plaintext \
  -d '{"config":{"command":["sh","-c","stty size && tty"]},"pty":{"cols":80,"rows":24}}' \
  localhost:9090 process.v1.ProcessService/Start
```

### Python (REST)

```python
import os
from pathlib import Path

import requests

REST = f"http://{os.environ['SANDBOXD_REST_ADDR']}/v1"

# Upload code over REST
requests.put(
    f"{REST}/files/main.py",
    data=Path("main.py").read_bytes(),
    headers={"Content-Type": "application/octet-stream"},
).raise_for_status()
```

Use `grpcurl` above for direct ProcessService calls, or use the supported
[Python SDK](#agent-sandbox-sdk-access), which bundles the generated gRPC
stubs and manages connectivity and sandbox lifecycle.

### Go

```go
conn, _ := grpc.NewClient(os.Getenv("SANDBOXD_GRPC_ADDR"),
	grpc.WithTransportCredentials(insecure.NewCredentials()))
client := processv1.NewProcessServiceClient(conn)
resp, _ := client.Execute(ctx, &processv1.ExecuteRequest{
	Config: &processv1.ProcessConfig{Command: []string{"python3", "main.py"}},
})
fmt.Println(resp.GetExitCode(), string(resp.GetStdout()))
```

## Agent Sandbox SDK access

`sandboxd` binds the pod network (`0.0.0.0` by default), but the SDKs do not
auto-detect it. They select the runtime and transport explicitly. The Go SDK
and synchronous Python SDK can use a direct **pod port-forward** for `:8080`
and `:9090`; the Go SDK can also dial the Pod IP or the Sandbox's headless
Service from inside the cluster. Filesystem calls use REST and `Run` uses
gRPC. The current `sandbox-router` cannot provide this combined transport
because it does not proxy gRPC.

> **Where commands run:** `ProcessService` executes commands inside the
> container that hosts `sandboxd` (see
> [Deployment topologies](#deployment-topologies)). With topology A the base
> `sandboxd` image is minimal (a shell + coreutils), so `Run` can use `sh`,
> `cat`, `ls`, etc. out of the box; for a language runtime, either build a
> sandboxd image that includes it (A) or inject sandboxd into an image that
> already has it (B). Files written over REST land in the `/workspace`
> volume.

**Go** — select the runtime via `Options`:

```go
sb, _ := sandbox.New(ctx, sandbox.Options{
    WarmPoolName: "my-pool",
    Runtime:      sandbox.RuntimeSandboxd, // pod port-forward; REST + gRPC
})
_ = sb.Open(ctx)
_ = sb.Write(ctx, "src/notes.txt", data)       // PUT /v1/files/...
res, _ := sb.Run(ctx, "cat src/notes.txt")     // gRPC ProcessService.Execute
_ = sb.Delete(ctx, "src", true)                // sandboxd-only
```

For an in-cluster caller, set `Connectivity` explicitly. Service connectivity
requires `spec.service: true` on the Sandbox template; Pod IP connectivity
does not, but it carries the risk of addressing a recycled Pod IP. Both modes
must also be allowed by the template's network policy. The default `Managed`
policy allows ingress only from `sandbox-router`, so direct clients require a
custom `spec.networkPolicy.ingress` rule that admits the caller (normally on
TCP ports `8080` and `9090`). Supplying `spec.networkPolicy` replaces all
secure defaults, so preserve every required ingress and egress rule. Use
`networkPolicyManagement: Unmanaged` only when another policy system provides
equivalent isolation.

```go
sb, _ := sandbox.New(ctx, sandbox.Options{
    WarmPoolName: "my-pool",
    Runtime:      sandbox.RuntimeSandboxd,
    Connectivity: sandbox.ConnectivityInClusterService,
})
```

**Python** — select the runtime via the connection config:

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig

client = SandboxClient(connection_config=SandboxdPodTunnelConnectionConfig())
sandbox = client.create_sandbox(warmpool="my-pool")
try:
    sandbox.files.write("src/notes.txt", b"hello\n")  # PUT /v1/files/...
    result = sandbox.commands.run("cat src/notes.txt")  # gRPC Execute
    sandbox.files.delete("src", recursive=True)        # sandboxd-only
finally:
    sandbox.terminate()
```

The Python gRPC path requires the `grpc` extra: `pip install k8s-agent-sandbox[grpc]`.

## Security model

- **Network containment:** both ports bind `0.0.0.0` by default so the daemon
  is reachable on the pod network through the Pod IP or a Service.
  Containment is provided by **pod isolation and NetworkPolicy**, not
  loopback binding; pass `--listen-host=127.0.0.1` to restrict to loopback
  for local development.
- **Transport authentication:** `sandboxd` does not authenticate clients or
  terminate TLS. Pod port-forward access is authorized by the Kubernetes API
  server. Protect direct pod-network access with NetworkPolicy, a service mesh
  or mTLS proxy, or another trusted transport boundary.
- **Path confinement:** every file path (and process `cwd`) is resolved with
  symlink evaluation and rejected unless it stays under `--root-dir`.
- **Metadata hygiene:** `/v1/metadata` only serves env vars matching
  `--metadata-env-prefix`, and names containing credential markers
  (`TOKEN`, `SECRET`, `PASSWORD`, `CREDENTIAL`, `KEY`) are always withheld.
  Never inject orchestrator credentials, Kubernetes API tokens, or cloud IAM
  keys into the sandbox environment.
- **Process hygiene:** children run in their own process groups; daemon
  shutdown SIGTERMs them, waits a grace period, then SIGKILLs stragglers.

## Local development

```bash
# Build
make build-sandboxd

# Run against a scratch workspace
mkdir -p /tmp/ws
bin/sandboxd --root-dir=/tmp/ws

# Test
go test ./packages/sandboxd/... -race
```
