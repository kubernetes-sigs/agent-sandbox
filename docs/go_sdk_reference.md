
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
- [type Sandbox](<#Sandbox>)
  - [func New\(\_ context.Context, opts Options\) \(\*Sandbox, error\)](<#New>)
  - [func \(s \*Sandbox\) Annotations\(\) map\[string\]string](<#Sandbox.Annotations>)
  - [func \(s \*Sandbox\) ClaimName\(\) string](<#Sandbox.ClaimName>)
  - [func \(s \*Sandbox\) Close\(ctx context.Context\) error](<#Sandbox.Close>)
  - [func \(s \*Sandbox\) Commands\(\) \*Commands](<#Sandbox.Commands>)
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
)
```

<a name="NewTracerProvider"></a>
### func [NewTracerProvider](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/tracing.go#L51>)

```go
func NewTracerProvider(ctx context.Context, serviceName string) (*sdktrace.TracerProvider, error)
```

NewTracerProvider creates a TracerProvider with an OTLP/gRPC exporter. The endpoint is read from OTEL\_EXPORTER\_OTLP\_ENDPOINT \(default: localhost:4317\). serviceName becomes the service.name resource attribute. The caller owns the returned provider and must call Shutdown when done.

<a name="CallOption"></a>
### type [CallOption](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L71>)

CallOption configures per\-call behavior for SDK operations.

```go
type CallOption func(*callOptions)
```

<a name="WithMaxAttempts"></a>
#### func [WithMaxAttempts](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L91>)

```go
func WithMaxAttempts(n int) CallOption
```

WithMaxAttempts sets the maximum number of attempts for an operation. Values ≤0 are ignored and the default is used \(1 for Run, 6 for file operations\).

```
result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
```

<a name="WithTimeout"></a>
#### func [WithTimeout](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L80>)

```go
func WithTimeout(d time.Duration) CallOption
```

WithTimeout sets the total timeout for a single operation, overriding the default RequestTimeout for that call.

<a name="Client"></a>
### type [Client](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L38-L48>)

Client manages sandbox lifecycles and tracks active handles.

```go
type Client struct {
    // contains filtered or unexported fields
}
```

<a name="NewClient"></a>
#### func [NewClient](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L51>)

```go
func NewClient(_ context.Context, opts Options) (*Client, error)
```

NewClient creates a Client with shared configuration.

<a name="Client.CreateSandbox"></a>
#### func \(\*Client\) [CreateSandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L80>)

```go
func (c *Client) CreateSandbox(ctx context.Context, warmPoolName, namespace string) (*Sandbox, error)
```

CreateSandbox provisions a new sandbox and returns a managed handle. On failure, the orphaned claim is cleaned up.

<a name="Client.DeleteAll"></a>
#### func \(\*Client\) [DeleteAll](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L219>)

```go
func (c *Client) DeleteAll(ctx context.Context)
```

DeleteAll closes and deletes all tracked sandboxes. Best\-effort.

<a name="Client.DeleteSandbox"></a>
#### func \(\*Client\) [DeleteSandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L201>)

```go
func (c *Client) DeleteSandbox(ctx context.Context, claimName, namespace string) error
```

DeleteSandbox closes the handle \(if tracked\) and deletes the claim.

<a name="Client.EnableAutoCleanup"></a>
#### func \(\*Client\) [EnableAutoCleanup](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L235>)

```go
func (c *Client) EnableAutoCleanup() (stop func())
```

EnableAutoCleanup calls DeleteAll on SIGINT/SIGTERM. Call the returned function to stop the signal handler.

<a name="Client.GetSandbox"></a>
#### func \(\*Client\) [GetSandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L112>)

```go
func (c *Client) GetSandbox(ctx context.Context, claimName, namespace string) (*Sandbox, error)
```

GetSandbox retrieves an existing sandbox by claim name. Returns the cached handle if connected, otherwise re\-attaches.

<a name="Client.ListActiveSandboxes"></a>
#### func \(\*Client\) [ListActiveSandboxes](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L169>)

```go
func (c *Client) ListActiveSandboxes() []Key
```

ListActiveSandboxes returns tracked sandboxes, pruning inactive handles.

<a name="Client.ListAllSandboxes"></a>
#### func \(\*Client\) [ListAllSandboxes](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L185>)

```go
func (c *Client) ListAllSandboxes(ctx context.Context, namespace string) ([]string, error)
```

ListAllSandboxes lists all SandboxClaim names in the given namespace.

<a name="Commands"></a>
### type [Commands](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/commands.go#L33-L41>)

Commands provides command execution on a sandbox.

```go
type Commands struct {
    // contains filtered or unexported fields
}
```

<a name="Commands.Run"></a>
#### func \(\*Commands\) [Run](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/commands.go#L52>)

```go
func (c *Commands) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
```

Run executes a command in the sandbox and returns the result. The combined JSON response \(stdout \+ stderr \+ metadata\) is limited to 16 MB; commands producing more output will fail with ErrResponseTooLarge.

Because command execution is not idempotent, Run defaults to a single attempt \(no retries\). For idempotent commands that should retry on transient server errors \(502, 503, etc.\), use WithMaxAttempts:

```
result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
```

<a name="ConnectionStrategy"></a>
### type [ConnectionStrategy](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go#L20-L23>)

ConnectionStrategy defines how the SDK discovers the sandbox\-router URL.

```go
type ConnectionStrategy interface {
    Connect(ctx context.Context) (baseURL string, err error)
    Close() error
}
```

<a name="DirectStrategy"></a>
### type [DirectStrategy](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go#L26-L28>)

DirectStrategy connects using a pre\-configured URL, bypassing all discovery.

```go
type DirectStrategy struct {
    URL string
}
```

<a name="DirectStrategy.Close"></a>
#### func \(\*DirectStrategy\) [Close](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go#L31>)

```go
func (s *DirectStrategy) Close() error
```



<a name="DirectStrategy.Connect"></a>
#### func \(\*DirectStrategy\) [Connect](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/strategy.go#L30>)

```go
func (s *DirectStrategy) Connect(_ context.Context) (string, error)
```



<a name="ExecutionResult"></a>
### type [ExecutionResult](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L126-L130>)

ExecutionResult holds the result of a command execution in the sandbox.

```go
type ExecutionResult struct {
    Stdout   string `json:"stdout"`
    Stderr   string `json:"stderr"`
    ExitCode int    `json:"exit_code"`
}
```

<a name="FileEntry"></a>
### type [FileEntry](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L141-L146>)

FileEntry represents a file or directory entry in the sandbox.

```go
type FileEntry struct {
    Name    string   `json:"name"`
    Size    int64    `json:"size"`
    Type    FileType `json:"type"`
    ModTime float64  `json:"mod_time"`
}
```

<a name="FileType"></a>
### type [FileType](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L133>)

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
### type [Files](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go#L72-L82>)

Files provides file operations on a sandbox.

```go
type Files struct {
    // contains filtered or unexported fields
}
```

<a name="Files.Exists"></a>
#### func \(\*Files\) [Exists](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go#L238>)

```go
func (f *Files) Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
```

Exists checks if a file or directory exists at the given path in the sandbox.

<a name="Files.List"></a>
#### func \(\*Files\) [List](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go#L189>)

```go
func (f *Files) List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
```

List returns the contents of a directory in the sandbox.

<a name="Files.Read"></a>
#### func \(\*Files\) [Read](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go#L145>)

```go
func (f *Files) Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
```

Read downloads a file from the sandbox.

<a name="Files.Write"></a>
#### func \(\*Files\) [Write](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/files.go#L90>)

```go
func (f *Files) Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
```

Write uploads content to the sandbox. The path must be a plain filename without directory separators \(e.g., "script.py", not "dir/script.py"\).

The entire content is buffered in memory as a multipart form body to support retries on transient failures. Content exceeding MaxUploadSize \(default 256 MB\) is rejected before any network I/O.

<a name="HTTPError"></a>
### type [HTTPError](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L56-L60>)

HTTPError represents a non\-OK HTTP response from the sandbox.

```go
type HTTPError struct {
    StatusCode int
    Body       string
    Operation  string
}
```

<a name="HTTPError.Error"></a>
#### func \(\*HTTPError\) [Error](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L62>)

```go
func (e *HTTPError) Error() string
```



<a name="Handle"></a>
### type [Handle](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L103-L114>)

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
### type [Info](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/types.go#L117-L123>)

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
### type [K8sHelper](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/k8s.go#L53-L62>)

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
#### func [NewK8sHelper](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/k8s.go#L67>)

```go
func NewK8sHelper(restConfig *rest.Config, log logr.Logger) (*K8sHelper, error)
```

NewK8sHelper creates a K8sHelper by loading kubeconfig and constructing all required clientsets. If restConfig is non\-nil it is used directly; otherwise in\-cluster config is tried first, then \~/.kube/config.

<a name="Key"></a>
### type [Key](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/client.go#L32-L35>)

Key identifies a tracked sandbox in the registry.

```go
type Key struct {
    Namespace string
    ClaimName string
}
```

<a name="Options"></a>
### type [Options](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/options.go#L44-L139>)

Options configures a Sandbox instance.

```go
type Options struct {
    // WarmPoolName is the name of the SandboxWarmPool to use. Required.
    // Must be a valid Kubernetes DNS subdomain (lowercase, [a-z0-9.-]).
    WarmPoolName string

    // Namespace where the SandboxClaim will be created. Default: "default".
    // Must be a valid Kubernetes DNS label (lowercase, [a-z0-9-]).
    Namespace string

    // GatewayName enables production mode. The client watches this Gateway resource
    // for an external IP, then routes through the sandbox-router.
    // Must be a valid Kubernetes DNS subdomain (lowercase, [a-z0-9.-]).
    GatewayName string

    // GatewayNamespace is where the Gateway lives. Default: "default".
    // Must be a valid Kubernetes DNS label (lowercase, [a-z0-9-]).
    GatewayNamespace string

    // GatewayScheme is the URL scheme used when constructing the base URL
    // from the Gateway's address. Default: "http".
    GatewayScheme string

    // APIURL enables advanced mode. The client connects directly to this URL,
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

<a name="Sandbox"></a>
### type [Sandbox](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L32-L56>)

Sandbox manages the lifecycle of a single agent\-sandbox instance. Operations are split across Commands and Files.

```go
type Sandbox struct {
    // contains filtered or unexported fields
}
```

<a name="New"></a>
#### func [New](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L66>)

```go
func New(_ context.Context, opts Options) (*Sandbox, error)
```

New creates a new Sandbox with the given options. Call Open\(\) to create a sandbox and establish connectivity.

<a name="Sandbox.Annotations"></a>
#### func \(\*Sandbox\) [Annotations](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L565>)

```go
func (s *Sandbox) Annotations() map[string]string
```



<a name="Sandbox.ClaimName"></a>
#### func \(\*Sandbox\) [ClaimName](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L541>)

```go
func (s *Sandbox) ClaimName() string
```



<a name="Sandbox.Close"></a>
#### func \(\*Sandbox\) [Close](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L371>)

```go
func (s *Sandbox) Close(ctx context.Context) error
```

Close deletes the SandboxClaim and cleans up resources.

<a name="Sandbox.Commands"></a>
#### func \(\*Sandbox\) [Commands](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L516>)

```go
func (s *Sandbox) Commands() *Commands
```

Commands returns the command execution sub\-object.

<a name="Sandbox.Disconnect"></a>
#### func \(\*Sandbox\) [Disconnect](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L474>)

```go
func (s *Sandbox) Disconnect(ctx context.Context) error
```

Disconnect closes the transport connection without deleting the SandboxClaim. The sandbox stays alive on the server. Call Open\(\) to reconnect. Disconnect is safe to call concurrently with Open; an in\-progress Open is cancelled before the transport is torn down.

<a name="Sandbox.Exists"></a>
#### func \(\*Sandbox\) [Exists](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L535>)

```go
func (s *Sandbox) Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
```



<a name="Sandbox.Files"></a>
#### func \(\*Sandbox\) [Files](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L519>)

```go
func (s *Sandbox) Files() *Files
```

Files returns the file operations sub\-object.

<a name="Sandbox.IsReady"></a>
#### func \(\*Sandbox\) [IsReady](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L511>)

```go
func (s *Sandbox) IsReady() bool
```

IsReady returns true if the sandbox is ready for communication.

<a name="Sandbox.List"></a>
#### func \(\*Sandbox\) [List](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L532>)

```go
func (s *Sandbox) List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
```



<a name="Sandbox.Open"></a>
#### func \(\*Sandbox\) [Open](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L176>)

```go
func (s *Sandbox) Open(ctx context.Context) (retErr error)
```

Open creates a SandboxClaim and waits for the sandbox to become ready, then discovers the API URL based on the configured connection mode. On failure after claim creation, the claim is automatically deleted; if deletion also fails, call Close\(\) to retry.

<a name="Sandbox.PodIP"></a>
#### func \(\*Sandbox\) [PodIP](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L559>)

```go
func (s *Sandbox) PodIP() string
```



<a name="Sandbox.PodName"></a>
#### func \(\*Sandbox\) [PodName](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L553>)

```go
func (s *Sandbox) PodName() string
```



<a name="Sandbox.Read"></a>
#### func \(\*Sandbox\) [Read](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L529>)

```go
func (s *Sandbox) Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
```



<a name="Sandbox.Run"></a>
#### func \(\*Sandbox\) [Run](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L523>)

```go
func (s *Sandbox) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
```



<a name="Sandbox.SandboxName"></a>
#### func \(\*Sandbox\) [SandboxName](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L547>)

```go
func (s *Sandbox) SandboxName() string
```



<a name="Sandbox.Write"></a>
#### func \(\*Sandbox\) [Write](<https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/go/sandbox/sandbox.go#L526>)

```go
func (s *Sandbox) Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
```



Generated by [gomarkdoc](<https://github.com/princjef/gomarkdoc>)
