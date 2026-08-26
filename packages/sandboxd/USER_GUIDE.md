# sandboxd User Guide

`sandboxd` is the portable sandbox runtime daemon defined by
[KEP-539.2](../../docs/keps/539.2-runtime-standardization/README.md). It runs
inside a sandbox pod and exposes the hybrid runtime API:

```text
sandboxd
├── gRPC  127.0.0.1:9090  →  ProcessService    (streaming process I/O)
└── HTTP  127.0.0.1:8080  →  FilesystemService (stateless file operations & runtime probes)
```

- **Process execution** is served over **gRPC** because `Start` is a
  long-lived server-streaming RPC: `stdout`/`stderr` flow continuously until
  the process exits.
- **Filesystem transfers** are served over **REST** so file bytes move as raw
  `application/octet-stream` payloads with no protobuf wrapping, and any
  plain HTTP client works without generated stubs.

Both listeners bind strictly to `localhost` inside the pod network namespace.
They are never reachable from outside the pod without explicit proxying
(`sandbox-router`).

The specifications live in [`spec/`](spec/):

| Surface | Spec |
|---|---|
| `ProcessService` (gRPC) | [`spec/process/v1/process.proto`](spec/process/v1/process.proto) |
| Filesystem & Runtime REST API | [`spec/filesystem/v1/filesystem.yaml`](spec/filesystem/v1/filesystem.yaml) |

## Endpoint discovery

SDKs and agent code discover the endpoints through environment variables set
on the workload container:

```bash
SANDBOXD_GRPC_ADDR=localhost:9090
SANDBOXD_REST_ADDR=localhost:8080
```

If the variables are absent, SDKs fall back to the legacy `python-runtime`
API, enabling a phased rollout across sandbox templates.

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

Requests are handled in parallel (no server-side serialization); clients do
not need their own locking for correctness:

- **Write vs. read** — writes are atomic (temp file + rename), so a
  concurrent reader sees either the complete previous file or the complete
  new one, never partial content.
- **Write vs. delete** — last operation wins: the path ends up present (new
  content) or absent, with no torn state.
- **Delete vs. read** — an in-flight download completes even if the file is
  deleted mid-transfer.
- **Concurrent writes to one path** — the last write wins atomically.

There is no cross-request transaction or compare-and-swap; agents that need
coordination on shared paths must layer it themselves.

## Side-loading into arbitrary workload containers

`sandboxd` is designed to be **side-loaded directly into arbitrary workload containers** at runtime.
This allows AI agents to execute processes and manage files directly within the target container's
environment (with its installed dependencies, packages, compilers, and PATH) without needing to
rebuild or bake `sandboxd` into the container image.

By mounting the `sandboxd` container image as an **Image Volume** and launching `sandboxd` in the background
via a Kubernetes `postStart` lifecycle hook, the workload container's original `ENTRYPOINT` and `CMD`
remain completely intact.

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata:
  name: sandboxd-template
spec:
  podTemplate:
    spec:
      volumes:
        - name: sandboxd-volume
          image:
            reference: us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:latest-main
            pullPolicy: IfNotPresent
      containers:
        - name: workload
          image: bring-your-own-image:latest  # Any arbitrary BYOI image
          stdin: true # Required for postStart command in cases where the command isn't specified in image
          # tty: true # Alternative to stdin if you prefer a TTY 
          env:
            - name: SANDBOXD_GRPC_ADDR
              value: "localhost:9090"
            - name: SANDBOXD_REST_ADDR
              value: "localhost:8080"
          ports:
            - containerPort: 8080   # REST (localhost-only; port documented for probes)
            - containerPort: 9090   # gRPC
          readinessProbe:
            httpGet:
              path: /v1/health
              port: 8080
          lifecycle:
            postStart:
              exec:
                command:
                  - "/bin/sh"
                  - "-c"
                  - "/opt/agent-sandbox/usr/local/bin/sandboxd --root-dir=/workspace --grpc-port=9090 --rest-port=8080 >/tmp/sandboxd.log 2>&1 &"
          volumeMounts:
            - name: sandboxd-volume
              mountPath: /opt/agent-sandbox
              readOnly: true
```

> **Why this works:**
> - **Bring-Your-Own-Image (BYOI):** `sandboxd` runs inside the workload container's root filesystem,
>   so commands executed via `ProcessService` have direct access to the workload's libraries, binaries,
>   and environment variables.
> - **Preserves Original Entrypoint:** Because `command` and `args` are omitted from the PodSpec,
>   Kubernetes executes the image's original `ENTRYPOINT` and `CMD` as PID 1.
> - **Zero Rebuilds:** The `sandboxd` image is mounted read-only via Kubernetes Image Volumes
>   (Kubernetes 1.31+), eliminating the need for custom base images or init container copy steps.

### Fallback: InitContainer & `emptyDir` (Kubernetes < 1.31)

For clusters where Image Volumes are not enabled by default, use an `initContainer` to copy the statically
compiled `sandboxd` binary into a shared `emptyDir` volume:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata:
  name: sandboxd-template
spec:
  podTemplate:
    spec:
      volumes:
        - name: sandboxd-bin
          emptyDir: {}
      initContainers:
        - name: inject-sandboxd
          image: us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:latest-main
          command: ["cp", "/usr/local/bin/sandboxd", "/sandboxd-bin/sandboxd"]
          volumeMounts:
            - name: sandboxd-bin
              mountPath: /sandboxd-bin
      containers:
        - name: workload
          image: bring-your-own-image:latest  # Any arbitrary BYOI image
          stdin: true # Required for postStart command in cases where the command isn't specified in image
          # tty: true # Alternative to stdin if you prefer a TTY 
          env:
            - name: SANDBOXD_GRPC_ADDR
              value: "localhost:9090"
            - name: SANDBOXD_REST_ADDR
              value: "localhost:8080"
          ports:
            - containerPort: 8080
            - containerPort: 9090
          readinessProbe:
            httpGet:
              path: /v1/health
              port: 8080
          lifecycle:
            postStart:
              exec:
                command:
                  - "/bin/sh"
                  - "-c"
                  - "/sandboxd-bin/sandboxd --root-dir=/workspace --grpc-port=9090 --rest-port=8080 >/tmp/sandboxd.log 2>&1 &"
          volumeMounts:
            - name: sandboxd-bin
              mountPath: /sandboxd-bin
```

## Flags

| Flag | Default | Description |
|---|---|---|
| `--grpc-port` | `9090` | Port for the gRPC ProcessService. Always binds to `127.0.0.1`. |
| `--rest-port` | `8080` | Port for the REST API. Always binds to `127.0.0.1`. |
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
curl -sf -X PUT --data-binary @local.py "localhost:8080/v1/files/src/main.py?mode=0644"

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

### Python

```python
import os

import grpc
import requests

from process.v1 import process_pb2, process_pb2_grpc  # generated from spec/process/v1/process.proto

REST = f"http://{os.environ['SANDBOXD_REST_ADDR']}/v1"
GRPC = os.environ["SANDBOXD_GRPC_ADDR"]

# Upload code over REST
requests.put(f"{REST}/files/main.py", data=open("main.py", "rb").read()).raise_for_status()

# Run it over gRPC
channel = grpc.insecure_channel(GRPC)
stub = process_pb2_grpc.ProcessServiceStub(channel)
resp = stub.Execute(process_pb2.ExecuteRequest(
    config=process_pb2.ProcessConfig(command=["python3", "main.py"])))
print(resp.exit_code, resp.stdout.decode())
```

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

## Security model

- **Network containment:** both ports bind to `127.0.0.1` only; external
  access requires explicit proxying through `sandbox-router`.
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
