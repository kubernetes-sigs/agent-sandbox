
## sandbox

```go
import "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
```

### Index

- [Constants](<#constants>)
- [Variables](<#variables>)
- [func NewTracerProvider\(ctx context.Context, serviceName string\) \(\*sdktrace.TracerProvider, error\)](<#NewTracerProvider>)
- [type CallOption](<#CallOption>)
  - [func WithMaxAttempts\(n int\) CallOption](<#WithMaxAttempts>)
  - [func WithTimeout\(d time.Duration\) CallOption](<#WithTimeout>)
- [type Client](<#Client>)
  - [func NewClient\(\_ context.Context, opts Options\) \(\*Client, error\)](<#NewClient>)
  - [func \(c \*Client\) CreateSandbox\(ctx context.Context, warmPoolName, namespace string\) \(\*Sandbox, error\)](<#Client.CreateSandbox>)
  - [func \(c \*Client\) DeleteAll\(ctx context.Context\)](<#Client.DeleteAll>)
  - [func \(c \*Client\) DeleteSandbox\(ctx context.Context, claimName, namespace string\) error](<#Client.DeleteSandbox>)
  - [func \(c \*Client\) EnableAutoCleanup\(\) \(stop func\(\)\)](<#Client.EnableAutoCleanup>)
  - [func \(c \*Client\) GetSandbox\(ctx context.Context, claimName, namespace string\) \(\*Sandbox, error\)](<#Client.GetSandbox>)
  - [func \(c \*Client\) ListActiveSandboxes\(\) \[\]Key](<#Client.ListActiveSandboxes>)
  - [func \(c \*Client\) ListAllSandboxes\(ctx context.Context, namespace string\) \(\[\]string, error\)](<#Client.ListAllSandboxes>)
- [type Commands](<#Commands>)
  - [func \(c \*Commands\) Run\(ctx context.Context, command string, opts ...CallOption\) \(\*ExecutionResult, error\)](<#Commands.Run>)
- [type ConnectionStrategy](<#ConnectionStrategy>)
- [type DirectStrategy](<#DirectStrategy>)
  - [func \(s \*DirectStrategy\) Close\(\) error](<#DirectStrategy.Close>)
  - [func \(s \*DirectStrategy\) Connect\(\_ context.Context\) \(string, error\)](<#DirectStrategy.Connect>)
- [type ExecutionResult](<#ExecutionResult>)
- [type FileEntry](<#FileEntry>)
- [type FileType](<#FileType>)
- [type Files](<#Files>)
  - [func \(f \*Files\) Delete\(ctx context.Context, path string, recursive bool, opts ...CallOption\) error](<#Files.Delete>)
  - [func \(f \*Files\) Exists\(ctx context.Context, path string, opts ...CallOption\) \(bool, error\)](<#Files.Exists>)
  - [func \(f \*Files\) List\(ctx context.Context, path string, opts ...CallOption\) \(\[\]FileEntry, error\)](<#Files.List>)
  - [func \(f \*Files\) Read\(ctx context.Context, path string, opts ...CallOption\) \(\[\]byte, error\)](<#Files.Read>)
  - [func \(f \*Files\) Write\(ctx context.Context, path string, content \[\]byte, opts ...CallOption\) error](<#Files.Write>)
- [type HTTPError](<#HTTPError>)
  - [func \(e \*HTTPError\) Error\(\) string](<#HTTPError.Error>)
- [type Handle](<#Handle>)
- [type Info](<#Info>)
- [type K8sHelper](<#K8sHelper>)
  - [func NewK8sHelper\(restConfig \*rest.Config, log logr.Logger\) \(\*K8sHelper, error\)](<#NewK8sHelper>)
- [type Key](<#Key>)
- [type Options](<#Options>)
- [type Runtime](<#Runtime>)
- [type Sandbox](<#Sandbox>)
  - [func New\(\_ context.Context, opts Options\) \(\*Sandbox, error\)](<#New>)
  - [func \(s \*Sandbox\) Annotations\(\) map\[string\]string](<#Sandbox.Annotations>)
  - [func \(s \*Sandbox\) ClaimName\(\) string](<#Sandbox.ClaimName>)
  - [func \(s \*Sandbox\) Close\(ctx context.Context\) error](<#Sandbox.Close>)
  - [func \(s \*Sandbox\) Commands\(\) \*Commands](<#Sandbox.Commands>)
  - [func \(s \*Sandbox\) Delete\(ctx context.Context, path string, recursive bool, opts ...CallOption\) error](<#Sandbox.Delete>)
  - [func \(s \*Sandbox\) Disconnect\(ctx context.Context\) error](<#Sandbox.Disconnect>)
  - [func \(s \*Sandbox\) Exists\(ctx context.Context, path string, opts ...CallOption\) \(bool, error\)](<#Sandbox.Exists>)
  - [func \(s \*Sandbox\) Files\(\) \*Files](<#Sandbox.Files>)
  - [func \(s \*Sandbox\) IsReady\(\) bool](<#Sandbox.IsReady>)
  - [func \(s \*Sandbox\) List\(ctx context.Context, path string, opts ...CallOption\) \(\[\]FileEntry, error\)](<#Sandbox.List>)
  - [func \(s \*Sandbox\) Open\(ctx context.Context\) \(retErr error\)](<#Sandbox.Open>)
  - [func \(s \*Sandbox\) PodIP\(\) string](<#Sandbox.PodIP>)
  - [func \(s \*Sandbox\) PodName\(\) string](<#Sandbox.PodName>)
  - [func \(s \*Sandbox\) Read\(ctx context.Context, path string, opts ...CallOption\) \(\[\]byte, error\)](<#Sandbox.Read>)
  - [func \(s \*Sandbox\) Run\(ctx context.Context, command string, opts ...CallOption\) \(\*ExecutionResult, error\)](<#Sandbox.Run>)
  - [func \(s \*Sandbox\) SandboxName\(\) string](<#Sandbox.SandboxName>)
  - [func \(s \*Sandbox\) Write\(ctx context.Context, path string, content \[\]byte, opts ...CallOption\) error](<#Sandbox.Write>)


### Constants

<a name="PodNameAnnotation"></a>

```go
const (

    // PodNameAnnotation is the annotation key on a Sandbox resource that
    // identifies the name of the underlying pod.
    PodNameAnnotation = "agents.x-k8s.io/pod-name"
)
```

### Variables

<a name="AttrClaimName"></a>Span attribute keys in the sandbox.\* namespace.

```go
var (
    AttrClaimName         = attribute.Key("sandbox.claim.name")
    AttrCommandExecutable = attribute.Key("sandbox.command.executable")
    AttrExitCode          = attribute.Key("sandbox.exit_code")
    AttrFilePath          = attribute.Key("sandbox.file.path")
    AttrFileSize          = attribute.Key("sandbox.file.size")
    AttrFileCount         = attribute.Key("sandbox.file.count")
    AttrFileExists        = attribute.Key("sandbox.file.exists")
    AttrGatewayName       = attribute.Key("sandbox.gateway.name")
    AttrGatewayNamespace  = attribute.Key("sandbox.gateway.namespace")
    AttrRequestID         = attribute.Key("sandbox.request_id")
)
```

<a name="ErrNotReady"></a>Sentinel errors returned by the SDK.

```go
var (
    ErrNotReady         = errors.New("sandbox is not ready")
    ErrTimeout          = errors.New("operation timed out")
    ErrClaimFailed      = errors.New("claim creation failed")
    ErrPortForwardDied  = errors.New("port-forward connection lost")
    ErrAlreadyOpen      = errors.New("sandbox is already open; call Close first")
    ErrOrphanedClaim    = errors.New("orphaned claim; call Close() to retry deletion")
    ErrRetriesExhausted = errors.New("retries exhausted")
    ErrSandboxDeleted   = errors.New("sandbox was deleted before becoming ready")
    ErrGatewayDeleted   = errors.New("gateway was deleted during address discovery")
    ErrResponseTooLarge = errors.New("response exceeded 16 MB limit")
    // ErrUnsupportedByRuntime is returned by operations the selected
    // runtime cannot perform (e.g. Delete on the legacy python-runtime).
    ErrUnsupportedByRuntime = errors.New("operation not supported by the sandbox runtime")
)
```

<a name="NewTracerProvider"></a>
### func [NewTracerProvider](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/tracing.go>)

```go
func NewTracerProvider(ctx context.Context, serviceName string) (*sdktrace.TracerProvider, error)
```

NewTracerProvider creates a TracerProvider with an OTLP/gRPC exporter. The endpoint is read from OTEL\_EXPORTER\_OTLP\_ENDPOINT \(default: localhost:4317\). serviceName becomes the service.name resource attribute. The caller owns the returned provider and must call Shutdown when done.

<a name="CallOption"></a>
### type [CallOption](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

CallOption configures per\-call behavior for SDK operations.

```go
type CallOption func(*callOptions)
```

<a name="WithMaxAttempts"></a>
#### func [WithMaxAttempts](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

```go
func WithMaxAttempts(n int) CallOption
```

WithMaxAttempts sets the maximum number of attempts for an operation. Values ≤0 are ignored and the default is used \(1 for Run, 6 for file operations\).

```
result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
```

<a name="WithTimeout"></a>
#### func [WithTimeout](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

```go
func WithTimeout(d time.Duration) CallOption
```

WithTimeout sets the total timeout for a single operation, overriding the default RequestTimeout for that call.

<a name="Client"></a>
### type [Client](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

Client manages sandbox lifecycles and tracks active handles.

```go
type Client struct {
    // contains filtered or unexported fields
}
```

<a name="NewClient"></a>
#### func [NewClient](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func NewClient(_ context.Context, opts Options) (*Client, error)
```

NewClient creates a Client with shared configuration.

<a name="Client.CreateSandbox"></a>
#### func \(\*Client\) [CreateSandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) CreateSandbox(ctx context.Context, warmPoolName, namespace string) (*Sandbox, error)
```

CreateSandbox provisions a new sandbox and returns a managed handle. On failure, the orphaned claim is cleaned up.

<a name="Client.DeleteAll"></a>
#### func \(\*Client\) [DeleteAll](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) DeleteAll(ctx context.Context)
```

DeleteAll closes and deletes all tracked sandboxes. Best\-effort.

<a name="Client.DeleteSandbox"></a>
#### func \(\*Client\) [DeleteSandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) DeleteSandbox(ctx context.Context, claimName, namespace string) error
```

DeleteSandbox closes the handle \(if tracked\) and deletes the claim.

<a name="Client.EnableAutoCleanup"></a>
#### func \(\*Client\) [EnableAutoCleanup](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) EnableAutoCleanup() (stop func())
```

EnableAutoCleanup calls DeleteAll on SIGINT/SIGTERM. Call the returned function to stop the signal handler.

<a name="Client.GetSandbox"></a>
#### func \(\*Client\) [GetSandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) GetSandbox(ctx context.Context, claimName, namespace string) (*Sandbox, error)
```

GetSandbox retrieves an existing sandbox by claim name. Returns the cached handle if connected, otherwise re\-attaches.

<a name="Client.ListActiveSandboxes"></a>
#### func \(\*Client\) [ListActiveSandboxes](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) ListActiveSandboxes() []Key
```

ListActiveSandboxes returns tracked sandboxes, pruning inactive handles.

<a name="Client.ListAllSandboxes"></a>
#### func \(\*Client\) [ListAllSandboxes](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

```go
func (c *Client) ListAllSandboxes(ctx context.Context, namespace string) ([]string, error)
```

ListAllSandboxes lists all SandboxClaim names in the given namespace.

<a name="Commands"></a>
### type [Commands](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/commands.go>)

Commands provides command execution on a sandbox.

```go
type Commands struct {
    // contains filtered or unexported fields
}
```

<a name="Commands.Run"></a>
#### func \(\*Commands\) [Run](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/commands.go>)

```go
func (c *Commands) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
```

Run executes a command in the sandbox and returns the result. The combined JSON response \(stdout \+ stderr \+ metadata\) is limited to 16 MB; commands producing more output will fail with ErrResponseTooLarge.

Because command execution is not idempotent, Run defaults to a single attempt \(no retries\). For idempotent commands that should retry on transient server errors \(502, 503, etc.\), use WithMaxAttempts:

```
result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
```

WithMaxAttempts applies only to the legacy runtime. With RuntimeSandboxd, Run issues a single gRPC Execute regardless of the configured attempts.

<a name="ConnectionStrategy"></a>
### type [ConnectionStrategy](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go>)

ConnectionStrategy defines how the SDK discovers the sandbox\-router URL.

```go
type ConnectionStrategy interface {
    Connect(ctx context.Context) (baseURL string, err error)
    Close() error
}
```

<a name="DirectStrategy"></a>
### type [DirectStrategy](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go>)

DirectStrategy connects using a pre\-configured URL, bypassing all discovery.

```go
type DirectStrategy struct {
    URL string
}
```

<a name="DirectStrategy.Close"></a>
#### func \(\*DirectStrategy\) [Close](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go>)

```go
func (s *DirectStrategy) Close() error
```



<a name="DirectStrategy.Connect"></a>
#### func \(\*DirectStrategy\) [Connect](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go>)

```go
func (s *DirectStrategy) Connect(_ context.Context) (string, error)
```



<a name="ExecutionResult"></a>
### type [ExecutionResult](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

ExecutionResult holds the result of a command execution in the sandbox.

```go
type ExecutionResult struct {
    Stdout   string `json:"stdout"`
    Stderr   string `json:"stderr"`
    ExitCode int    `json:"exit_code"`
}
```

<a name="FileEntry"></a>
### type [FileEntry](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

FileEntry represents a file or directory entry in the sandbox. It is runtime\-neutral: the SDK decodes the legacy wire format \(mod\_time as a float POSIX timestamp\) and the sandboxd wire format \(modified\_at as an RFC 3339 string, plus mode\) into this one shape.

```go
type FileEntry struct {
    Name    string
    Size    int64
    Type    FileType
    ModTime time.Time
    // Mode holds octal permission bits (e.g. "0644"). Only populated by
    // the sandboxd runtime; empty on legacy.
    Mode string
}
```

<a name="FileType"></a>
### type [FileType](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

FileType represents the type of a file entry.

```go
type FileType string
```

<a name="FileTypeFile"></a>

```go
const (
    FileTypeFile      FileType = "file"
    FileTypeDirectory FileType = "directory"
)
```

<a name="Files"></a>
### type [Files](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go>)

Files provides file operations on a sandbox.

```go
type Files struct {
    // contains filtered or unexported fields
}
```

<a name="Files.Delete"></a>
#### func \(\*Files\) [Delete](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go>)

```go
func (f *Files) Delete(ctx context.Context, path string, recursive bool, opts ...CallOption) error
```

Delete removes a file or directory in the sandbox. When recursive is true, directories are removed with their contents \(rm \-rf semantics\); otherwise deleting a non\-empty directory fails with a 409 HTTPError.

Only supported by the sandboxd runtime: the legacy python\-runtime has no delete endpoint, and calls return ErrUnsupportedByRuntime.

<a name="Files.Exists"></a>
#### func \(\*Files\) [Exists](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go>)

```go
func (f *Files) Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
```

Exists checks if a file or directory exists at the given path in the sandbox.

<a name="Files.List"></a>
#### func \(\*Files\) [List](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go>)

```go
func (f *Files) List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
```

List returns the contents of a directory in the sandbox.

<a name="Files.Read"></a>
#### func \(\*Files\) [Read](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go>)

```go
func (f *Files) Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
```

Read downloads a file from the sandbox.

<a name="Files.Write"></a>
#### func \(\*Files\) [Write](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go>)

```go
func (f *Files) Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
```

Write uploads content to the sandbox.

With the legacy runtime the path must be a plain filename without directory separators \(e.g., "script.py", not "dir/script.py"\). The sandboxd runtime supports relative paths and creates parent directories automatically.

The entire content is buffered in memory to support retries on transient failures. Content exceeding MaxUploadSize \(default 256 MB\) is rejected before any network I/O.

<a name="HTTPError"></a>
### type [HTTPError](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

HTTPError represents a non\-OK HTTP response from the sandbox.

```go
type HTTPError struct {
    StatusCode int
    Body       string
    Operation  string
}
```

<a name="HTTPError.Error"></a>
#### func \(\*HTTPError\) [Error](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

```go
func (e *HTTPError) Error() string
```



<a name="Handle"></a>
### type [Handle](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

Handle provides high\-level interaction with a sandbox instance. Sandbox implements this interface; consumers should accept Handle in their APIs to enable testing with mocks. For sub\-object access \(Commands\(\), Files\(\)\), use the concrete \*Sandbox type directly.

```go
type Handle interface {
    Open(ctx context.Context) error
    Close(ctx context.Context) error
    Disconnect(ctx context.Context) error
    IsReady() bool

    Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
    Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
    Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
    List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
    Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
}
```

<a name="Info"></a>
### type [Info](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go>)

Info provides read\-only access to sandbox identity metadata.

```go
type Info interface {
    ClaimName() string
    SandboxName() string
    PodName() string
    PodIP() string
    Annotations() map[string]string
}
```

<a name="K8sHelper"></a>
### type [K8sHelper](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/k8s.go>)

K8sHelper encapsulates all Kubernetes API interactions for sandbox lifecycle management. It can be shared across multiple Sandbox instances.

```go
type K8sHelper struct {
    AgentsClient     agentsv1beta1.AgentsV1beta1Interface
    ExtensionsClient extensionsv1beta1.ExtensionsV1beta1Interface
    DynamicClient    dynamic.Interface
    CoreClient       corev1client.CoreV1Interface
    DiscoveryClient  discoveryv1client.DiscoveryV1Interface
    RestConfig       *rest.Config

    Log logr.Logger
}
```

<a name="NewK8sHelper"></a>
#### func [NewK8sHelper](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/k8s.go>)

```go
func NewK8sHelper(restConfig *rest.Config, log logr.Logger) (*K8sHelper, error)
```

NewK8sHelper creates a K8sHelper by loading kubeconfig and constructing all required clientsets. If restConfig is non\-nil it is used directly; otherwise in\-cluster config is tried first, then \~/.kube/config.

<a name="Key"></a>
### type [Key](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go>)

Key identifies a tracked sandbox in the registry.

```go
type Key struct {
    Namespace string
    ClaimName string
}
```

<a name="Options"></a>
### type [Options](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/options.go>)

Options configures a Sandbox instance.

```go
type Options struct {
    // WarmPoolName is the name of the SandboxWarmPool to use.
    // Required in Options before calling Open() to provision a new sandbox.
    // Optional on Client-level Options; pass the pool name per-sandbox to
    // CreateSandbox instead.
    // Must be a valid Kubernetes DNS subdomain (lowercase, [a-z0-9.-]).
    WarmPoolName string

    // Runtime selects the in-sandbox runtime API. Default: RuntimeLegacyPython.
    // RuntimeSandboxd connects via a pod port-forward, so GatewayName is not
    // supported with it. APIURL remains available as an advanced/testing
    // escape hatch for the REST endpoint.
    Runtime Runtime

    // SandboxdRESTPort is the pod port of sandboxd's Filesystem & Runtime
    // REST API. Only used with RuntimeSandboxd. Default: 8080.
    SandboxdRESTPort int

    // SandboxdGRPCPort is the pod port of sandboxd's gRPC ProcessService.
    // Only used with RuntimeSandboxd. Default: 9090.
    SandboxdGRPCPort int

    // Namespace where the SandboxClaim will be created. Default: "default".
    // Must be a valid Kubernetes DNS label (lowercase, [a-z0-9-]).
    Namespace string

    // GatewayName enables Gateway mode. The client watches this Gateway resource
    // for an external IP, then routes through the sandbox-router.
    // Must be a valid Kubernetes DNS subdomain (lowercase, [a-z0-9.-]).
    GatewayName string

    // GatewayNamespace is where the Gateway lives. Default: "default".
    // Must be a valid Kubernetes DNS label (lowercase, [a-z0-9-]).
    GatewayNamespace string

    // GatewayScheme is the URL scheme used when constructing the base URL
    // from the Gateway's address. Default: "http".
    GatewayScheme string

    // APIURL enables direct connection mode. The client connects directly to this URL,
    // bypassing gateway discovery. Takes precedence over GatewayName.
    APIURL string

    // ServerPort is the port the sandbox runtime listens on. Default: 8888.
    ServerPort int

    // SandboxReadyTimeout is how long to wait for the sandbox to become ready. Default: 180s.
    SandboxReadyTimeout time.Duration

    // GatewayReadyTimeout is how long to wait for the gateway IP. Default: 180s.
    GatewayReadyTimeout time.Duration

    // PortForwardReadyTimeout is how long to wait for port-forward to be established. Default: 30s.
    PortForwardReadyTimeout time.Duration

    // CleanupTimeout is how long to wait for claim deletion during both Open
    // rollback and Close. Uses a detached context so cleanup succeeds even if
    // the caller's context is already cancelled. Default: 30s.
    CleanupTimeout time.Duration

    // RequestTimeout is the total timeout for a single SDK method call
    // (e.g., Run, Read, Write), encompassing all retry attempts and backoff
    // sleeps. Applied only when the caller's context has no deadline.
    // Default: 180s.
    RequestTimeout time.Duration

    // PerAttemptTimeout bounds the time to receive response headers per
    // HTTP attempt. Stopped on success so body reads use RequestTimeout.
    // Default: 60s.
    PerAttemptTimeout time.Duration

    // MaxDownloadSize is the maximum response body size for Read().
    // Run() uses a fixed 16 MB decode limit; List() and Exists() use a
    // fixed 8 MB internal limit. Default: 256 MB.
    MaxDownloadSize int64

    // MaxUploadSize is the maximum content size for Write(). Content
    // exceeding this limit is rejected before any network I/O. Default: 256 MB.
    MaxUploadSize int64

    // Logger for structured logging. Defaults to stderr at INFO level.
    // Provide a custom logr.Logger for full control, or set Quiet to
    // suppress output.
    Logger logr.Logger

    // Quiet suppresses the default stderr logger. Has no effect when a
    // custom Logger is provided (non-zero-value).
    Quiet bool

    // K8sHelper provides pre-constructed Kubernetes clients. If nil, a new
    // K8sHelper is created from RestConfig. Use this to share clients
    // across multiple Sandbox instances.
    K8sHelper *K8sHelper

    // RestConfig overrides the Kubernetes REST config. If nil, the client first
    // tries in-cluster config (for pods), then falls back to the default
    // kubeconfig (~/.kube/config or KUBECONFIG env). Ignored when K8sHelper is set.
    RestConfig *rest.Config

    // HTTPTransport overrides the HTTP transport for sandbox operations.
    // If nil, a default transport with sensible timeouts is created.
    // Use this for custom TLS, proxies, or other transport-level settings.
    HTTPTransport http.RoundTripper

    // TraceServiceName is the OpenTelemetry service name used for the tracer's
    // instrumentation scope and the resource's service.name attribute.
    // Default: "sandbox-client".
    TraceServiceName string

    // TracerProvider sets the OpenTelemetry TracerProvider for span creation.
    // If nil, falls back to otel.GetTracerProvider (noop by default).
    TracerProvider trace.TracerProvider
}
```

<a name="Runtime"></a>
### type [Runtime](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/options.go>)

Runtime identifies the in\-sandbox runtime API the SDK speaks.

```go
type Runtime string
```

<a name="RuntimeLegacyPython"></a>

```go
const (
    // RuntimeLegacyPython is the python-runtime HTTP API (POST /upload,
    // GET /download|list|exists/{path}, POST /execute on port 8888),
    // reached through the sandbox-router. Default.
    RuntimeLegacyPython Runtime = "legacy-python"
    // RuntimeSandboxd is the sandboxd hybrid API defined by KEP-539.2:
    // REST filesystem (/v1/files/...) on port 8080 plus gRPC
    // ProcessService on port 9090. The SDK connects over a pod port-forward
    // to the sandbox pod.
    RuntimeSandboxd Runtime = "sandboxd"
)
```

<a name="Sandbox"></a>
### type [Sandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

Sandbox manages the lifecycle of a single agent\-sandbox instance. Operations are split across Commands and Files.

```go
type Sandbox struct {
    // contains filtered or unexported fields
}
```

<a name="New"></a>
#### func [New](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func New(_ context.Context, opts Options) (*Sandbox, error)
```

New creates a new Sandbox with the given options. Call Open\(\) to create a sandbox and establish connectivity.

<a name="Sandbox.Annotations"></a>
#### func \(\*Sandbox\) [Annotations](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Annotations() map[string]string
```



<a name="Sandbox.ClaimName"></a>
#### func \(\*Sandbox\) [ClaimName](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) ClaimName() string
```



<a name="Sandbox.Close"></a>
#### func \(\*Sandbox\) [Close](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Close(ctx context.Context) error
```

Close deletes the SandboxClaim and cleans up resources.

<a name="Sandbox.Commands"></a>
#### func \(\*Sandbox\) [Commands](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Commands() *Commands
```

Commands returns the command execution sub\-object.

<a name="Sandbox.Delete"></a>
#### func \(\*Sandbox\) [Delete](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Delete(ctx context.Context, path string, recursive bool, opts ...CallOption) error
```

Delete removes a file or directory \(sandboxd runtime only; the legacy python\-runtime returns ErrUnsupportedByRuntime\). Not part of the Handle interface to avoid breaking existing implementers.

<a name="Sandbox.Disconnect"></a>
#### func \(\*Sandbox\) [Disconnect](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Disconnect(ctx context.Context) error
```

Disconnect closes the transport connection without deleting the SandboxClaim. The sandbox stays alive on the server. Call Open\(\) to reconnect. Disconnect is safe to call concurrently with Open; an in\-progress Open is cancelled before the transport is torn down.

<a name="Sandbox.Exists"></a>
#### func \(\*Sandbox\) [Exists](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
```



<a name="Sandbox.Files"></a>
#### func \(\*Sandbox\) [Files](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Files() *Files
```

Files returns the file operations sub\-object.

<a name="Sandbox.IsReady"></a>
#### func \(\*Sandbox\) [IsReady](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) IsReady() bool
```

IsReady returns true if the sandbox is ready for communication.

<a name="Sandbox.List"></a>
#### func \(\*Sandbox\) [List](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
```



<a name="Sandbox.Open"></a>
#### func \(\*Sandbox\) [Open](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Open(ctx context.Context) (retErr error)
```

Open creates a SandboxClaim and waits for the sandbox to become ready, then discovers the API URL based on the configured connection mode. On failure after claim creation, the claim is automatically deleted; if deletion also fails, call Close\(\) to retry.

<a name="Sandbox.PodIP"></a>
#### func \(\*Sandbox\) [PodIP](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) PodIP() string
```



<a name="Sandbox.PodName"></a>
#### func \(\*Sandbox\) [PodName](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) PodName() string
```



<a name="Sandbox.Read"></a>
#### func \(\*Sandbox\) [Read](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
```



<a name="Sandbox.Run"></a>
#### func \(\*Sandbox\) [Run](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
```



<a name="Sandbox.SandboxName"></a>
#### func \(\*Sandbox\) [SandboxName](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) SandboxName() string
```



<a name="Sandbox.Write"></a>
#### func \(\*Sandbox\) [Write](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go>)

```go
func (s *Sandbox) Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
```



Generated by [gomarkdoc](<https://github.com/princjef/gomarkdoc>)
