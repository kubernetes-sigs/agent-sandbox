# Go Client SDK for Agent Sandbox

This Go client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It handles the full SandboxClaim lifecycle
(creation, readiness, cleanup) so callers only need to think about running commands and
transferring files.

It supports two in-sandbox runtimes (`RuntimeLegacyPython` and `RuntimeSandboxd`) and multiple
connectivity modes: **Port-Forward** (`ConnectivityPortForward`), **In-Cluster Service DNS**
(`ConnectivityInClusterService`), **In-Cluster Pod IP** (`ConnectivityInClusterPodIP`),
**Gateway** (Kubernetes Gateway API), and **Direct URL** (`APIURL`).

## Architecture

### Runtimes (`Options.Runtime`)

- **`RuntimeLegacyPython` (default):** Speaks the `python-runtime` HTTP API on `ServerPort` (default `:8888`). Reached through `sandbox-router` by default, or directly on the pod when using in-cluster connectivity.
- **`RuntimeSandboxd`:** Speaks the `sandboxd` hybrid API defined by KEP-539.2 — REST filesystem, health, and metadata (`/v1/files/...`, `/v1/health`, `/v1/metadata`) on `SandboxdRESTPort` (default `:8080`) plus gRPC `ProcessService` on `SandboxdGRPCPort` (default `:9090`). Reached directly on the sandbox pod (via pod port-forward or in-cluster connectivity, without `sandbox-router`).

### Connectivity Modes (`Options.Connectivity`, `GatewayName`, `APIURL`)

1. **Port-Forward Mode (`ConnectivityPortForward`, default):** Uses `client-go/tools/portforward` natively (no `kubectl` binary required; ideal for local development and CI). With `RuntimeLegacyPython`, tunnels to the `sandbox-router` Service; with `RuntimeSandboxd`, tunnels directly to the sandbox Pod's REST and gRPC ports.
2. **In-Cluster Service Mode (`ConnectivityInClusterService`):** Dials the Sandbox's headless Service by its in-cluster DNS name (`Status.ServiceFQDN`), taking the API server and `sandbox-router` off the data path. Requires `spec.service: true` on the template and never silently falls back to Pod IP, preventing Pod IP reuse across tenants.
3. **In-Cluster Pod IP Mode (`ConnectivityInClusterPodIP`):** Dials `Status.PodIP` directly from inside the cluster without requiring a headless Service. Prefer `ConnectivityInClusterService` when sandboxes cross a trust boundary.
4. **Gateway Mode (`GatewayName`):** Traffic flows from Client -> Cloud Load Balancer (Gateway) -> Router Service -> Sandbox Pod (`RuntimeLegacyPython` only). The client watches the Gateway resource for an external IP.
5. **Direct URL Mode (`APIURL`):** The client connects directly to a provided `APIURL`, bypassing discovery. Useful for custom domains or in-cluster router URLs.

## Prerequisites

- A running Kubernetes cluster with a valid kubeconfig (or in-cluster config). This is required even in Direct URL or in-cluster modes because the client creates Kubernetes clientsets for `SandboxClaim` lifecycle management.
- The [**Agent Sandbox Controller**](https://github.com/kubernetes-sigs/agent-sandbox?tab=readme-ov-file#installation) installed.
- The **Sandbox Router** deployed in the target namespace when using router-based modes (`RuntimeLegacyPython` with default port-forward, `GatewayName`, or router `APIURL`) — see [sandbox-router](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router/README.md) and its [deployment manifests](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router/deploy). *(Note: If you are using a specific tagged release, replace `main` in these URLs with your version tag. `RuntimeSandboxd` and in-cluster connectivity modes talk directly to the sandbox pod and do not require `sandbox-router`.)*
- A `SandboxWarmPool` created in the target namespace.
- Go 1.26+.

## Installation

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox
```

## Versioning and Releases

The Go SDK is currently published from the repository's root Go module.
That means repository tags such as `v0.1.0` are also the SDK versions for:

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox@v0.1.0
```

To follow the most recent repository release, use:

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox@latest
```

## Usage Examples

### 1. Gateway Mode

Use this when running against a cluster with a public Gateway IP. The client automatically
discovers the Gateway address.

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{
    GatewayName:      "external-http-gateway",
    GatewayNamespace: "default",
})
if err != nil { log.Fatal(err) }
defer client.DeleteAll(ctx)

sb, err := client.CreateSandbox(ctx, "my-sandbox-pool", "default")
if err != nil { log.Fatal(err) }

result, err := sb.Run(ctx, "echo 'Hello from Cloud!'")
if err != nil { log.Fatal(err) }
fmt.Println(result.Stdout)
```

### 2. Port-Forward Mode

Use this for local development or CI. If you omit `GatewayName`, `APIURL`, and `Connectivity`,
the client defaults to `ConnectivityPortForward` and establishes an SPDY port-forward tunnel
automatically.

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{})
if err != nil { log.Fatal(err) }
defer client.DeleteAll(ctx)

sb, err := client.CreateSandbox(ctx, "my-sandbox-pool", "default")
if err != nil { log.Fatal(err) }

result, err := sb.Run(ctx, "echo 'Hello from Local!'")
if err != nil { log.Fatal(err) }
fmt.Println(result.Stdout)
```

### 3. `sandboxd` Runtime and In-Cluster Connectivity

Select `RuntimeSandboxd` to use the `sandboxd` daemon's REST filesystem (`:8080`) and gRPC
`ProcessService` (`:9090`). By default (`ConnectivityPortForward`), the SDK port-forwards
directly to the sandbox pod. When running inside the cluster, set `Connectivity` to
`ConnectivityInClusterService` (requires `spec.service: true` on the template) or
`ConnectivityInClusterPodIP`:

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{
    Runtime:      sandbox.RuntimeSandboxd,
    Connectivity: sandbox.ConnectivityInClusterService, // or ConnectivityPortForward / ConnectivityInClusterPodIP
})
if err != nil { log.Fatal(err) }
defer client.DeleteAll(ctx)

sb, err := client.CreateSandbox(ctx, "my-sandboxd-pool", "default")
if err != nil { log.Fatal(err) }

if err := sb.Write(ctx, "src/notes.txt", []byte("hello\n")); err != nil {
    log.Fatal(err)
}
result, err := sb.Run(ctx, "cat src/notes.txt")
if err != nil { log.Fatal(err) }
fmt.Println(result.Stdout)

if err := sb.Delete(ctx, "src", true); err != nil {
    log.Fatal(err)
}
```

### 4. Direct URL Mode

Use `APIURL` to bypass discovery entirely. Useful for:

- **Internal Agents:** Running inside the cluster via the router Service DNS.
- **Custom Domains:** Connecting via HTTPS (e.g., `https://sandbox.example.com`).

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{
    APIURL: "http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080",
})
if err != nil { log.Fatal(err) }
defer client.DeleteAll(ctx)

sb, err := client.CreateSandbox(ctx, "my-sandbox-pool", "default")
if err != nil { log.Fatal(err) }

entries, err := sb.List(ctx, ".")
if err != nil { log.Fatal(err) }
fmt.Println(entries)
```

### 5. Custom Ports

If your legacy sandbox runtime listens on a port other than 8888, specify `ServerPort`. For
`RuntimeSandboxd`, customize `SandboxdRESTPort` (default 8080) and `SandboxdGRPCPort`
(default 9090).

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{
    ServerPort: 3000,
})
```

### File Operations

```go
// Write a file. On RuntimeLegacyPython, the path must be a plain filename without
// directory separators (e.g., "script.py", not "dir/script.py"). On RuntimeSandboxd,
// relative paths like "src/script.py" are supported and parent directories are created
// automatically.
err := sb.Write(ctx, "script.py", []byte("print('hello')"))

// Stream a large file without buffering it in memory. Streaming uploads use
// one request attempt because an io.Reader cannot generally be replayed.
// Legacy runtime uploads use HTTP chunked transfer encoding, so the runtime
// and any proxy in front of it must accept requests without Content-Length.
// (The example requires imports for os and log.)
file, err := os.Open("model.bin")
if err != nil { log.Fatal(err) }
defer file.Close()
if err := sb.WriteReader(ctx, "model.bin", file); err != nil {
    log.Fatal(err)
}

// Read a file
data, err := sb.Read(ctx, "script.py")

// Stream a large download into a caller-owned destination. ReadTo never closes
// the destination and writes at most MaxDownloadSize bytes.
download, err := os.Create("model-copy.bin")
if err != nil { log.Fatal(err) }
defer download.Close()
written, err := sb.ReadTo(ctx, "model.bin", download)
if err != nil { log.Fatal(err) }
fmt.Printf("downloaded %d bytes\n", written)

// Check existence
exists, err := sb.Exists(ctx, "script.py")

// Delete a file or directory (RuntimeSandboxd only; returns ErrUnsupportedByRuntime on legacy)
err = sb.Delete(ctx, "script.py", false)
```

`Read()` and `ReadTo()` responses are capped by `MaxDownloadSize` (256 MB by
default). `Run()` responses are capped at 16 MB; `List()`/`Exists()` at 8 MB.

### Runtime Health and Metadata

With `RuntimeSandboxd`, query the in-sandbox daemon (`GET /v1/health` and
`GET /v1/metadata`). The legacy runtime returns `ErrUnsupportedByRuntime`.
Neither call retries unless you pass `WithMaxAttempts`.

```go
health, err := sb.Health(ctx)   // health.Status, health.UptimeSeconds
meta, err := sb.Metadata(ctx)   // meta.Env (non-sensitive, SANDBOX_-prefixed by default)
```

### 6. Custom TLS / Transport

If your Gateway uses HTTPS with a private CA, provide a custom transport:

```go
tlsConfig := &tls.Config{RootCAs: myCAPool}
client, err := sandbox.NewClient(ctx, sandbox.Options{
    GatewayName:   "external-https-gateway",
    GatewayScheme: "https",
    HTTPTransport: &http.Transport{TLSClientConfig: tlsConfig},
})
```

### Multi-Sandbox Management

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{})
stop := client.EnableAutoCleanup() // cleanup on SIGINT/SIGTERM
defer stop()
defer client.DeleteAll(ctx)

sb1, _ := client.CreateSandbox(ctx, "python-pool", "default")
sb2, _ := client.CreateSandbox(ctx, "node-pool", "default")

// List tracked sandboxes
for _, key := range client.ListActiveSandboxes() {
    fmt.Printf("  %s/%s\n", key.Namespace, key.ClaimName)
}

// Re-attach to existing sandbox by claim name
sb, _ := client.GetSandbox(ctx, sb1.ClaimName(), "default")

// Label the claims a client creates (Options.Labels), then list by label
labeled, err := sandbox.NewClient(ctx, sandbox.Options{Labels: map[string]string{"app": "agent"}})
if err != nil { log.Fatal(err) }
defer labeled.DeleteAll(ctx)

sb3, err := labeled.CreateSandbox(ctx, "python-pool", "default")
if err != nil { log.Fatal(err) }

names, err := labeled.ListAllSandboxes(ctx, "default", sandbox.WithLabelSelector("app=agent"))
if err != nil { log.Fatal(err) }
fmt.Println(names) // includes sb3.ClaimName()

// Expire claims on their own (Options.ShutdownAfter)
ttl, err := sandbox.NewClient(ctx, sandbox.Options{ShutdownAfter: time.Hour})
if err != nil { log.Fatal(err) }
defer ttl.DeleteAll(ctx)
```

## Configuration

All options are documented on the `Options` struct in
[options.go](sandbox/options.go). Key fields:

- `WarmPoolName`: passed per-sandbox to `CreateSandbox` (or set on `Options` when calling `sandbox.New` directly).
- `Runtime`: selects the in-sandbox runtime API — `RuntimeLegacyPython` (default) or `RuntimeSandboxd`.
- `Connectivity`: selects the transport — `ConnectivityPortForward` (default), `ConnectivityInClusterService`, or `ConnectivityInClusterPodIP`.
- `SandboxdRESTPort` / `SandboxdGRPCPort`: pod ports for `RuntimeSandboxd` (defaults: `8080` and `9090`).
- `Env`: environment variables to inject into the `SandboxClaim`. Setting this
  forces a cold start from the warm pool template instead of adopting a
  pre-warmed pod, which may increase startup latency.
- `Labels`: labels added to every `SandboxClaim` the client creates (queryable via `ListAllSandboxes` with `WithLabelSelector`).
- `ShutdownAfter`: expire every claim this client creates after this long, so a
  crashed client does not leak sandboxes. Unset by default (no expiry).
- `GatewayName`: set to enable Gateway mode (`RuntimeLegacyPython` only).
- `APIURL`: set for Direct URL mode (takes precedence over `GatewayName`).
- `TracerProvider`: OpenTelemetry integration.

Any operation accepts `WithTimeout` to override the default request timeout,
or `WithMaxAttempts` to control retry behavior:

```go
result, err := client.Run(ctx, "make build", sandbox.WithTimeout(10*time.Minute))
```

`Run` also takes `WithEnv` and `WithWorkingDir` (RuntimeSandboxd only; the
legacy runtime returns `ErrUnsupportedByRuntime`). `WithEnv` is merged over the
sandbox's environment, and the directory is relative to the sandbox root:

```go
result, err := client.Run(ctx, "make build",
    sandbox.WithEnv(map[string]string{"CI": "1"}),
    sandbox.WithWorkingDir("project"))
```

## Retry Behavior

File operations (`Read`, `Write`, `List`, `Exists`, `Delete`) are automatically retried (up to
6 attempts) on 500/502/503/504 responses and connection errors with exponential backoff.
`WriteReader` streams from an `io.Reader` with a single request attempt because a
reader cannot generally be replayed safely after a partial upload. Passing
`WithMaxAttempts(n)` with `n > 1` returns an error rather than silently reducing
the operation to a single attempt.

`ReadTo` can retry before a successful response begins. Once response bytes have
been written to the destination, a body-read or destination-write failure is
returned without retrying because the destination cannot generally be rewound.
The caller owns the destination and should decide whether to keep or remove any
partially written data.

**Important:** `Run()` defaults to a single attempt (no retries) because command
execution is not idempotent. On `RuntimeLegacyPython`, use `WithMaxAttempts` to
opt in to retries for idempotent commands (`RuntimeSandboxd` always issues a
single gRPC `Execute`):

```go
result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
```

## Disconnect / Reconnect

`Disconnect()` closes the transport connection **without deleting** the
SandboxClaim. The sandbox stays alive on the server. Call `Open()` to
reconnect to the same sandbox:

```go
client.Disconnect(ctx) // transport closed, claim preserved
// ... later ...
client.Open(ctx)       // reconnects to the same sandbox
```

This is useful for suspending a session (e.g., between user requests in a
web service) while keeping the sandbox warm. `Close()` deletes the claim;
`Disconnect(ctx)` preserves it.

## Timeouts and Context

`Open()` executes several sequential phases (claim creation, sandbox
readiness, transport connection), each bounded by its own timeout
(`SandboxReadyTimeout`, `GatewayReadyTimeout`, `PortForwardReadyTimeout`).
**Pass a context with a deadline** to bound the total `Open()` duration:

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
defer cancel()
if err := client.Open(ctx); err != nil { ... }
```

| Option | Default | Governs |
|--------|---------|---------|
| `SandboxReadyTimeout` | 180 s | Waiting for the sandbox to become ready |
| `GatewayReadyTimeout` | 180 s | Waiting for the gateway IP |
| `PortForwardReadyTimeout` | 30 s | Establishing the SPDY tunnel |
| `CleanupTimeout` | 30 s | Claim deletion during rollback / Close |
| `RequestTimeout` | 180 s | Total timeout per SDK method call (Run, Read, …) |
| `PerAttemptTimeout` | 60 s | Time to receive response headers per attempt |
| `MaxUploadSize` | 256 MB | Maximum content size for `Write()` and `WriteReader()` |
| `MaxDownloadSize` | 256 MB | Maximum response body size for `Read()` and `ReadTo()` |

## Port-Forward Recovery

In port-forward mode, a background monitor detects tunnel death and clears the
client's ready state. Subsequent operations fail immediately with `ErrNotReady`
(wrapping `ErrPortForwardDied`) instead of timing out.

To recover, call `Open()` again. The client will verify the claim and sandbox
still exist, then establish a new tunnel:

```go
result, err := client.Run(ctx, "echo hi")
if errors.Is(err, sandbox.ErrNotReady) {
    // Port-forward died; reconnect.
    if reconnErr := client.Open(ctx); reconnErr != nil {
        if errors.Is(reconnErr, sandbox.ErrSandboxDeleted) {
            // Claim was deleted externally; start fresh.
            reconnErr = client.Open(ctx)
        } else if errors.Is(reconnErr, sandbox.ErrOrphanedClaim) {
            // Sandbox no longer ready or verification failed; clean up and start fresh.
            client.Close(ctx)
            reconnErr = client.Open(ctx)
        }
        if reconnErr != nil {
            log.Fatal("reconnect failed:", reconnErr)
        }
    }
    result, err = client.Run(ctx, "echo hi")
}
```

If `Close()` fails to delete the claim (e.g., API server unavailable), the client
preserves the claim name so `Close()` can be retried to clean up the orphaned claim.
Calling `Open()` on a client with an orphaned claim returns `ErrOrphanedClaim`.

## Error Reference

| Error | Meaning |
|-------|---------|
| `ErrNotReady` | Client is not open or transport died. Call `Open()`. |
| `ErrAlreadyOpen` | `Open()` called on an already-open client. Call `Close()` first. |
| `ErrOrphanedClaim` | A previous claim could not be cleaned up (failed `Close()`, failed `Open()` rollback, or sandbox disappeared during reconnect); call `Close()` to retry deletion. |
| `ErrTimeout` | Sandbox or Gateway did not become ready within the configured timeout. |
| `ErrClaimFailed` | SandboxClaim creation was rejected by the API server, or the claim reported a failure the controller does not retry (for example `InvalidMetadata` or `ClaimExpired`). |
| `ErrWarmPoolNotFound` | The claim's SandboxWarmPool does not exist. |
| `ErrTemplateNotFound` | The SandboxTemplate behind the warm pool does not exist. |
| `ErrPortForwardDied` | The SPDY tunnel dropped. Call `Open()` to reconnect. |
| `ErrNoSandboxService` | `ConnectivityInClusterService` was selected, but the Sandbox has no headless Service (`spec.service: true` is not set on the template). |
| `ErrRetriesExhausted` | All HTTP retry attempts failed. |
| `ErrSandboxDeleted` | The Sandbox was deleted before becoming ready. |
| `ErrGatewayDeleted` | The Gateway was deleted during address discovery. |
| `ErrResponseTooLarge` | Response body exceeded the 16 MB decode limit for `Run()`. |
| `ErrUnsupportedByRuntime` | Operation is not supported by the selected runtime or transport (e.g., `Delete`, `Health`, or `Metadata` on `RuntimeLegacyPython`, or `Run` with `RuntimeSandboxd` and `APIURL`). |

Non-OK HTTP responses are wrapped in `*HTTPError`, which can be extracted
with `errors.As` to inspect the status code:

```go
var httpErr *sandbox.HTTPError
if errors.As(err, &httpErr) {
    fmt.Printf("status %d: %s\n", httpErr.StatusCode, httpErr.Body)
}
```

## Testing / Mocking

The package exports two interfaces:

- **`Handle`**: the core API (`Open`, `Close`, `Disconnect(ctx)`, `Run`, `Read`, `Write`,
  `List`, `Exists`, `IsReady`). Accept this in your APIs to enable testing with fakes. For
  sub-object access (`Commands()`, `Files()`), use the concrete `*Sandbox` type directly.
- **`Info`**: read-only identity accessors (`ClaimName`, `SandboxName`,
  `PodName`, `PodIP`, `Annotations`). These are on the concrete `*Sandbox` (and the
  `Info` interface) rather than `Handle`, so adding new accessors (such as
  `ServiceFQDN()` on `*Sandbox`) is not a breaking change for mock implementors.

```go
// Accept the narrow Handle interface for testability.
func ProcessInSandbox(ctx context.Context, sb sandbox.Handle) error {
    if err := sb.Open(ctx); err != nil {
        return err
    }
    defer sb.Close(context.Background())
    result, err := sb.Run(ctx, "echo hello")
    // ...
}

// When you need identity metadata, accept the concrete type or Info.
func LogSandboxIdentity(info sandbox.Info) {
    log.Printf("claim=%s sandbox=%s pod=%s", info.ClaimName(), info.SandboxName(), info.PodName())
}
```

## Running Tests

### Unit Tests

```bash
go test ./clients/go/sandbox/ -v -count=1
```

### Integration Tests

Integration tests require a running cluster with the Agent Sandbox controller and a
`SandboxWarmPool` installed. They are behind the `integration` build tag.

```bash
# Dev mode (port-forward)
INTEGRATION_TEST=1 go test ./clients/go/sandbox/ -tags=integration -v -timeout=300s

# Gateway mode
go test ./clients/go/sandbox/ -tags=integration -v -timeout=300s \
    -args --gateway-name=external-http-gateway --gateway-namespace=default

# Direct URL mode
go test ./clients/go/sandbox/ -tags=integration -v -timeout=300s \
    -args --api-url=http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080
```
