# sandboxd User Guide

`sandboxd` is the portable sandbox runtime daemon defined by
[KEP-539.2](../../docs/keps/539.2-runtime-standardization/README.md). It runs
inside a sandbox pod and exposes the hybrid runtime API:

```text
sandboxd (sidecar)
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

## Deploying as a sidecar

`sandboxd` runs as a sidecar next to your (unmodified) workload container.
The two share the workspace volume; the workload reaches `sandboxd` over pod
loopback.

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: sandboxd-template
spec:
  podTemplate:
    spec:
      containers:
        - name: sandboxd
          image: us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:latest-main
          ports:
            - containerPort: 8080   # REST (localhost-only; port documented for probes)
            - containerPort: 9090   # gRPC
          readinessProbe:
            httpGet:
              path: /v1/health
              port: 8080
          volumeMounts:
            - name: workspace
              mountPath: /workspace
        - name: workload
          image: your-agent-image:latest
          env:
            - name: SANDBOXD_GRPC_ADDR
              value: "localhost:9090"
            - name: SANDBOXD_REST_ADDR
              value: "localhost:8080"
          volumeMounts:
            - name: workspace
              mountPath: /workspace
      volumes:
        - name: workspace
          emptyDir: {}
```

> **Note:** commands launched through `ProcessService` execute inside the
> `sandboxd` container, with the shared `/workspace` volume as their working
> directory — so files written by either container are visible to both.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--grpc-addr` | `127.0.0.1:9090` | gRPC ProcessService listen address (keep on localhost). |
| `--rest-addr` | `127.0.0.1:8080` | REST API listen address (keep on localhost). |
| `--root-dir` | `/workspace` | Sandbox root confining all file operations and working directories. |
| `--metadata-env-prefix` | `SANDBOX_` | Env var prefix exposed on `/v1/metadata`. |
| `--shutdown-timeout` | `10s` | Grace period for in-flight requests and child processes. |
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
