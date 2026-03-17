# Go Client SDK for Agent Sandbox

This Go client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It handles the full SandboxClaim lifecycle
(creation, readiness, cleanup) so callers only need to think about running commands and
transferring files.

It supports three connectivity modes: **Gateway** (Kubernetes Gateway API), **Port-Forward**
(native SPDY tunnel), and **Direct URL** (in-cluster or custom domain).

## Architecture

The client operates in three modes:

1. **Production (Gateway Mode):** Traffic flows from the Client -> Cloud Load Balancer (Gateway)
   -> Router Service -> Sandbox Pod. The client watches the Gateway resource for an external IP.
2. **Development (Port-Forward Mode):** Traffic flows from the Client -> SPDY tunnel -> Router
   Service -> Sandbox Pod. Uses `client-go/tools/portforward` natively — no `kubectl` required.
3. **Advanced / Internal Mode:** The client connects directly to a provided `APIURL`, bypassing
   discovery. Useful for in-cluster agents or custom domains.

## Prerequisites

- A running Kubernetes cluster with a valid kubeconfig (or in-cluster config). This is required even in Direct URL mode because the client creates Kubernetes clientsets for SandboxClaim lifecycle management.
- The [**Agent Sandbox Controller**](https://github.com/kubernetes-sigs/agent-sandbox?tab=readme-ov-file#installation) installed.
- The **Sandbox Router** deployed in the target namespace (`sandbox-router-svc`).
- A `SandboxTemplate` created in the target namespace.
- Go 1.26+.

## Installation

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox
```

## Usage Examples

### 1. Production Mode (Gateway)

Use this when running against a cluster with a public Gateway IP. The client automatically
discovers the Gateway address.

```go
client, err := sandbox.NewClient(sandbox.Options{
    TemplateName:     "my-sandbox-template",
    GatewayName:      "external-http-gateway",
    GatewayNamespace: "default",
    Namespace:        "default",
})
if err != nil { log.Fatal(err) }
defer client.Close(context.Background())

ctx := context.Background()
if err := client.Open(ctx); err != nil { log.Fatal(err) }

result, err := client.Run(ctx, "echo 'Hello from Cloud!'")
if err != nil { log.Fatal(err) }
fmt.Println(result.Stdout)
```

### 2. Developer Mode (Port-Forward)

Use this for local development or CI. If you omit `GatewayName` and `APIURL`, the client
automatically establishes an SPDY port-forward tunnel to the Router Service.

```go
client, err := sandbox.NewClient(sandbox.Options{
    TemplateName: "my-sandbox-template",
    Namespace:    "default",
})
if err != nil { log.Fatal(err) }
defer client.Close(context.Background())

ctx := context.Background()
if err := client.Open(ctx); err != nil { log.Fatal(err) }

result, err := client.Run(ctx, "echo 'Hello from Local!'")
if err != nil { log.Fatal(err) }
fmt.Println(result.Stdout)
```

### 3. Advanced / Internal Mode

Use `APIURL` to bypass discovery entirely. Useful for:

- **Internal Agents:** Running inside the cluster (connect via K8s DNS).
- **Custom Domains:** Connecting via HTTPS (e.g., `https://sandbox.example.com`).

```go
client, err := sandbox.NewClient(sandbox.Options{
    TemplateName: "my-sandbox-template",
    APIURL:       "http://sandbox-router-svc.default.svc.cluster.local:8080",
    Namespace:    "default",
})
if err != nil { log.Fatal(err) }
defer client.Close(context.Background())

ctx := context.Background()
if err := client.Open(ctx); err != nil { log.Fatal(err) }

entries, err := client.List(ctx, ".")
if err != nil { log.Fatal(err) }
fmt.Println(entries)
```

### 4. Custom Ports

If your sandbox runtime listens on a port other than 8888, specify `ServerPort`.

```go
client, err := sandbox.NewClient(sandbox.Options{
    TemplateName: "my-sandbox-template",
    ServerPort:   3000,
})
```

### File Operations

```go
// Write a file (only the base filename is sent; directory components are discarded).
// Paths like "", ".", "..", and "/" are rejected with an error.
err := client.Write(ctx, "script.py", []byte("print('hello')"))

// Read a file
data, err := client.Read(ctx, "script.py")

// Check existence
exists, err := client.Exists(ctx, "script.py")
```

### 5. Custom TLS / Transport

If your Gateway uses HTTPS with a private CA, provide a custom transport:

```go
tlsConfig := &tls.Config{RootCAs: myCAPool}
client, err := sandbox.NewClient(sandbox.Options{
    TemplateName:  "my-sandbox-template",
    GatewayName:   "external-https-gateway",
    GatewayScheme: "https",
    HTTPTransport: &http.Transport{TLSClientConfig: tlsConfig},
})
```

## Configuration

All options are documented on the `Options` struct in
[options.go](sandbox/options.go). Key fields:

- `TemplateName` *(required)* — name of the `SandboxTemplate`.
- `GatewayName` — set to enable Gateway mode.
- `APIURL` — set for Direct URL mode (takes precedence over `GatewayName`).
- `EnableTracing` / `TracerProvider` — OpenTelemetry integration.

Any operation accepts `WithTimeout` to override the default request timeout:

```go
result, err := client.Run(ctx, "make build", sandbox.WithTimeout(10*time.Minute))
```

## Retry Behavior

Operations are automatically retried on 5xx responses and connection errors with
exponential backoff. See constants in [transport.go](sandbox/transport.go) for details.

## Port-Forward Recovery

In port-forward mode, a background monitor detects tunnel death and clears the
client's ready state. Subsequent operations fail immediately with `ErrNotReady`
(wrapping `ErrPortForwardDied`) instead of timing out.

To recover, call `Open()` again — the client will verify the claim and sandbox
still exist, then establish a new tunnel:

```go
result, err := client.Run(ctx, "echo hi")
if errors.Is(err, sandbox.ErrNotReady) {
    // Port-forward died; reconnect.
    if reconnErr := client.Open(ctx); reconnErr != nil {
        if errors.Is(reconnErr, sandbox.ErrOrphanedClaim) {
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

## Error Sentinel Reference

| Error | Meaning |
|-------|---------|
| `ErrNotReady` | Client is not open or transport died. Call `Open()`. |
| `ErrAlreadyOpen` | `Open()` called on an already-open client. Call `Close()` first. |
| `ErrOrphanedClaim` | A previous claim could not be cleaned up (failed `Close()`, failed `Open()` rollback, or sandbox disappeared during reconnect); call `Close()` to retry deletion. |
| `ErrTimeout` | Sandbox or Gateway did not become ready within the configured timeout. |
| `ErrClaimFailed` | SandboxClaim creation was rejected by the API server. |
| `ErrPortForwardDied` | The SPDY tunnel dropped. Call `Open()` to reconnect. |
| `ErrRetriesExhausted` | All HTTP retry attempts failed. |
| `ErrSandboxDeleted` | The Sandbox was deleted before becoming ready. |
| `ErrGatewayDeleted` | The Gateway was deleted during address discovery. |

## Testing / Mocking

The package exports two interfaces:

- **`Client`** — the core API (`Open`, `Close`, `Run`, `Read`, `Write`, `List`,
  `Exists`, `IsReady`). Accept this in your APIs to enable testing with fakes.
- **`SandboxInfo`** — read-only identity accessors (`ClaimName`, `SandboxName`,
  `PodName`, `Annotations`). These are on the concrete `*SandboxClient` (and the
  `SandboxInfo` interface) rather than `Client`, so adding new accessors is not
  a breaking change for mock implementors.

```go
// Accept the narrow Client interface for testability.
func ProcessInSandbox(ctx context.Context, sb sandbox.Client) error {
    if err := sb.Open(ctx); err != nil {
        return err
    }
    defer sb.Close(context.Background())
    result, err := sb.Run(ctx, "echo hello")
    // ...
}

// When you need identity metadata, accept the concrete type or SandboxInfo.
func LogSandboxIdentity(info sandbox.SandboxInfo) {
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
`SandboxTemplate` installed. They are behind the `integration` build tag.

```bash
# Dev mode (port-forward)
INTEGRATION_TEST=1 go test ./clients/go/sandbox/ -tags=integration -v -timeout=300s

# Gateway mode
go test ./clients/go/sandbox/ -tags=integration -v -timeout=300s \
    -args --gateway-name=external-http-gateway --gateway-namespace=default

# Direct URL mode
go test ./clients/go/sandbox/ -tags=integration -v -timeout=300s \
    -args --api-url=http://sandbox-router:8080
```
