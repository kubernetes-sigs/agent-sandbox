# Go Client SDK for Agent Sandbox

This Go client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It handles the full SandboxClaim lifecycle
(creation, readiness, cleanup) so callers only need to think about running commands and
transferring files.

It supports three connectivity modes: **Gateway** (Kubernetes Gateway API), **Port-Forward**
(native SPDY tunnel), and **Direct URL** (in-cluster or custom domain).

## Architecture

The client operates in three connectivity modes:

1. **Gateway Mode:** Traffic flows from the Client -> Cloud Load Balancer (Gateway)
   -> Router Service -> Sandbox Pod. The client watches the Gateway resource for an external IP.
2. **Port-Forward Mode:** Traffic flows from the Client -> SPDY tunnel -> Router
   Service -> Sandbox Pod. Uses `client-go/tools/portforward` natively, no `kubectl` required (ideal for local development and CI).
3. **Direct URL Mode:** The client connects directly to a provided `APIURL`, bypassing
   discovery. Useful for in-cluster agents or custom domains.

## Prerequisites

- A running Kubernetes cluster with a valid kubeconfig (or in-cluster config). This is required even in Direct URL mode because the client creates Kubernetes clientsets for SandboxClaim lifecycle management.
- The [**Agent Sandbox Controller**](https://github.com/kubernetes-sigs/agent-sandbox?tab=readme-ov-file#installation) installed.
- The **Sandbox Router** deployed in the target namespace, or in another namespace named by `RouterNamespace` (see [sandbox-router](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router/README.md) and its [deployment manifests](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router/deploy)). *(Note: If you are using a specific tagged release, replace `main` in these URLs with your version tag.)*
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

Use this for local development or CI. If you omit `GatewayName` and `APIURL`, the client
automatically establishes an SPDY port-forward tunnel to the Router Service.

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

### 3. Direct URL Mode

Use `APIURL` to bypass discovery entirely. Useful for:

- **Internal Agents:** Running inside the cluster (connect via K8s DNS).
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

### 4. Custom Ports

If your sandbox runtime listens on a port other than 8888, specify `ServerPort`.

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{
    ServerPort: 3000,
})
```

### File Operations

```go
// Write a file (must be a plain filename, no directory separators).
// Paths like "dir/script.py" are rejected with an error.
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
```

`Read()` and `ReadTo()` responses are capped by `MaxDownloadSize` (256 MB by
default). `Run()` responses are capped at 16 MB; `List()`/`Exists()` at 8 MB.

### Runtime Health and Metadata

With `RuntimeSandboxd`, query the in-sandbox daemon. The legacy runtime returns
`ErrUnsupportedByRuntime`. Neither call retries unless you pass
`WithMaxAttempts`.

```go
health, err := sb.Health(ctx)   // health.Status, health.UptimeSeconds
meta, err := sb.Metadata(ctx)   // meta.Env (non-sensitive, SANDBOX_-prefixed by default)
```

### Streaming Commands (sandboxd)

`sb.Commands().Start` runs a command with the same shell-string semantics as
`Run` (`/bin/sh -c`), but lets you read output before the command exits. It needs
`RuntimeSandboxd` and a gRPC connection established through pod port-forward or
in-cluster connectivity. The legacy runtime, a REST-only `APIURL`, and
`ConnectivityRouterPortForward` return `ErrUnsupportedByRuntime`. That mode
port-forwards the sandbox-router HTTP server and can carry the sandbox HTTP
API, not gRPC. This does not add a Gateway+sandboxd connection mode.

The example assumes `sb` is already open with `RuntimeSandboxd`. Import
`context`, `io`, `os`, `time`, and the `sandbox` SDK package:

```go
func streamCommand(ctx context.Context, sb *sandbox.Sandbox) (int, error) {
    handle, err := sb.Commands().Start(ctx,
        "echo starting; sleep 1; echo done",
        sandbox.WithTimeout(10*time.Minute))
    if err != nil { return 0, err }
    defer handle.Close()

    exitCode := 0
    for {
        event, err := handle.Recv()
        if err == io.EOF { return exitCode, nil }
        if err != nil { return 0, err }
        switch event.Type {
        case sandbox.ProcessEventStdout:
            if _, err := os.Stdout.Write(event.Data); err != nil { return 0, err }
        case sandbox.ProcessEventStderr:
            if _, err := os.Stderr.Write(event.Data); err != nil { return 0, err }
        case sandbox.ProcessEventExit:
            exitCode = event.ExitCode
        }
    }
}
```

`Start` waits for the daemon's initial process ID, not the first output. A short
command may already have exited by then. `ProcessID()` is a daemon-local virtual
ID, not an OS PID or a durable identity across daemon restarts.

Output events contain raw byte chunks, not necessarily complete lines or valid
UTF-8. stdout and stderr remain separate and have no guaranteed global ordering;
the SDK does not accumulate output. A nonzero exit code is a command result,
not an SDK error. Continue reading **after the exit event to `io.EOF`** to check
the final RPC status. Missing required events are protocol errors, and wrapped
gRPC errors retain their status code.

The caller context and a positive `WithTimeout` bound the **whole stream**,
including startup. `Start` does not apply the default `RequestTimeout` and does
not have a separate startup-only timeout. Close the handle or cancel its context
when you stop reading; `Close` is idempotent and cancels only this session, not
the shared connection or SandboxClaim. A nil Close error is local cleanup, not
confirmation that the remote process has already been killed. This is not detach.

To request graceful shutdown from another goroutine, keep the Start context
alive and give the signal RPC its own bounded context:

```go
signalCtx, cancelSignal := context.WithTimeout(ctx, 5*time.Second)
defer cancelSignal()
err := handle.SendSignal(signalCtx, sandbox.SignalSIGTERM)
```

Only `SignalSIGINT`, `SignalSIGTERM`, and `SignalSIGKILL` are supported.
Signal success is not an exit acknowledgement; continue the original receive
loop through Exit + EOF. Signal failures, or cancelling only `signalCtx`, do not
cancel that loop. SIGINT/SIGTERM may be trapped or ignored: the original process
deadline or `Close` provides a final cancellation boundary. Do not cancel Start
before trying to send a graceful-stop signal. The handle uses its original
connection and process identity even if the Sandbox later reconnects.

Each handle permits one active `Recv`; a concurrent second call returns
`ErrConcurrentRecv` without ending the session. Signals and Close may run
concurrently with the receive loop. Explicit cancellation, timeout, handle Close,
and the parent Sandbox's Close/Disconnect release session resources even if
nobody calls Recv again. Parent Close still deletes the claim; Disconnect does
not. The SDK does not read, drain or discard output in the background, so a
caller that stops consuming without cancelling can retain resources and block
the process through backpressure. Unconsumed ordinary EOF or transport faults
are not guaranteed to be discovered automatically.

`Start` never retries non-idempotent process creation and rejects
`WithMaxAttempts(n)` when `n > 1`. Transport attempts before a request reaches
the service are not a guarantee of repeated command execution. stdin, PTY,
reattachment, and automatic process recreation are not provided by this API.

### 5. Custom TLS / Transport

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

- `WarmPoolName`: passed per-sandbox to `CreateSandbox`.
- `Env`: environment variables to inject into the `SandboxClaim`. Setting this
  forces a cold start from the warm pool template instead of adopting a
  pre-warmed pod, which may increase startup latency.
- `Labels`: labels added to every `SandboxClaim` the client creates.
- `ShutdownAfter`: expire every claim this client creates after this long, so a
  crashed client does not leak sandboxes. Unset by default (no expiry).
- `Connectivity`: `port-forward` (default), `router-port-forward`,
  `in-cluster-service`, or `in-cluster-pod-ip`. `router-port-forward`
  port-forwards the sandbox-router HTTP server so it can proxy to the sandbox
  HTTP port. sandboxd `Run` and `Start` need gRPC and are not available on that
  path. It cannot reach sandboxd when authentication is enabled (the router
  strips `Authorization`, see #2102), and each request is capped by the
  router's `ProxyTimeout`. Set `DisablePodIPRouting: true` to route by cache
  or DNS
  instead of a stored pod IP when the router has `--cache-enabled` or the
  Sandbox has a headless Service.
- `RouterNamespace`: namespace of `sandbox-router-svc` for the port-forward
  modes that go through the router. Defaults to the claim namespace. Set it to
  `agent-sandbox-system` when using the shipped router manifests, which deploy
  the router there.
- `GatewayName`: set to enable Gateway mode.
- `APIURL`: set for Direct URL mode (takes precedence over `GatewayName`).
- `TracerProvider`: OpenTelemetry integration.

Operations accept `WithTimeout` for a total operation timeout. HTTP operations
override their default request timeout; sandboxd gRPC commands use the caller
context unless an explicit timeout is supplied. Retry options depend on the
operation: in particular, streaming `Start` rejects attempts greater than one.

```go
result, err := client.Run(ctx, "make build", sandbox.WithTimeout(10*time.Minute))
```

`Run` and `Commands.Start` also take `WithEnv` and `WithWorkingDir`
(RuntimeSandboxd only; the legacy runtime returns `ErrUnsupportedByRuntime`).
`WithEnv` is merged over the sandbox's environment, and the directory is relative
to the sandbox root:

```go
result, err := client.Run(ctx, "make build",
    sandbox.WithEnv(map[string]string{"CI": "1"}),
    sandbox.WithWorkingDir("project"))
```

## Retry Behavior

File operations (`Read`, `Write`, `List`, `Exists`) are automatically retried (up to
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
execution is not idempotent. On the legacy runtime, use `WithMaxAttempts` to opt
in to retries for idempotent commands; sandboxd `Run` always sends one Execute
RPC. Streaming `Commands.Start` rejects attempts greater than one:

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
| `ErrRetriesExhausted` | All HTTP retry attempts failed. |
| `ErrSandboxDeleted` | The Sandbox was deleted before becoming ready. |
| `ErrGatewayDeleted` | The Gateway was deleted during address discovery. |
| `ErrProcessProtocol` | A streaming command returned invalid events or ended without the required Init/Exit. Missing-event EOF also matches `io.ErrUnexpectedEOF`. |
| `ErrConcurrentRecv` | Another receive call is active on this process handle; the session remains open. |

Streaming RPC and signal failures preserve their gRPC status code through
wrapping; inspect them with `status.Code(err)` from `google.golang.org/grpc/status`.

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
  `PodName`, `Annotations`). These are on the concrete `*Sandbox` (and the
  `Info` interface) rather than `Handle`, so adding new accessors is not
  a breaking change for mock implementors.

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
