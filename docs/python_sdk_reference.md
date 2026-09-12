<a id="k8s_agent_sandbox.sandbox_client"></a>

## k8s\_agent\_sandbox.sandbox\_client

This module provides the SandboxClient for interacting with the Agentic Sandbox.
It handles lifecycle management (claiming, waiting) and interaction (execution,
file I/O) via the Sandbox resource handle.

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient"></a>

### SandboxClient Objects

```python
class SandboxClient(Generic[T])
```

A registry-based client for managing Sandbox lifecycles.
Tracks all active handles to ensure flat code structure and safe cleanup.

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.sandbox_class"></a>

##### sandbox\_class

type: ignore

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.__init__"></a>

##### \_\_init\_\_

```python
def __init__(connection_config: SandboxConnectionConfig | None = None,
             tracer_config: SandboxTracerConfig | None = None,
             cleanup: bool = False)
```

Initializes the SandboxClient.

**Arguments**:

- `connection_config` - Configuration for connecting to the sandboxes.
  Defaults to SandboxLocalTunnelConnectionConfig() which uses
  kubectl port-forwarding. Can also be SandboxDirectConnectionConfig
  or SandboxGatewayConnectionConfig.
- `tracer_config` - Configuration for OpenTelemetry tracing.
  Defaults to an empty SandboxTracerConfig (tracing disabled).
- `cleanup` - If True, registers an atexit hook to automatically delete
  managed sandboxes when the program terminates. This includes
  internally named and reattached claims; explicitly named claims
  created by this client remain caller-owned. Defaults to False.

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.create_sandbox"></a>

##### create\_sandbox

```python
def create_sandbox(warmpool: str,
                   namespace: str = "default",
                   sandbox_ready_timeout: int = 180,
                   labels: dict[str, str] | None = None,
                   *,
                   claim_name: str | None = None,
                   adopt_existing: bool = False,
                   shutdown_after_seconds: int | None = None,
                   volume_claim_templates: list[dict] | None = None,
                   pod_labels: dict[str, str] | None = None,
                   pod_annotations: dict[str, str] | None = None,
                   env: dict[str, str] | None = None) -> T
```

Provisions new Sandbox claim and returns a Sandbox handle which tracks
the underlying infrastructure.

**Arguments**:

- `warmpool` - Name of the SandboxWarmPool to use.
- `namespace` - Kubernetes namespace for the claim.
- `sandbox_ready_timeout` - Seconds to wait for the sandbox to be ready.
- `labels` - Optional Kubernetes labels to attach to the claim object
  (``SandboxClaim.metadata.labels``).
- `claim_name` - Optional deterministic SandboxClaim name. When omitted,
  the client preserves its random-name behavior and owns automatic
  cleanup. Explicitly named claims remain caller-owned.
- `adopt_existing` - On a create conflict, adopt the exact existing
  ``claim_name`` only after validating its immutable request
  contract. Requires ``claim_name`` and cannot be combined with
  ``shutdown_after_seconds``.
- `shutdown_after_seconds` - Optional TTL in seconds. When set, the
  claim's ``spec.lifecycle`` is populated with a ``shutdownTime``
  of *now + shutdown_after_seconds* (UTC) and a ``shutdownPolicy``
  of ``"Delete"``, so the controller auto-deletes the claim on
  expiry. Must be a positive integer.
- `volume_claim_templates` - Optional list of volume claim templates
  to override/merge with the sandbox template.
- `pod_labels` - Optional labels stamped onto the running Sandbox **Pod**
  via ``spec.additionalPodMetadata.labels``. Unlike ``labels``
  (which land on the claim object), these are readable from inside
  the sandbox through the Downward API.
- `pod_annotations` - Optional annotations stamped onto the running
  Sandbox **Pod** via ``spec.additionalPodMetadata.annotations``.
- `env` - Optional environment variables to inject into the SandboxClaim.
  Setting this populates ``spec.env`` and forces a cold start
  from the warm pool template instead of adopting a pre-warmed
  pod, which may increase startup latency.
  

**Example**:

  
  >>> client = SandboxClient()
  >>> sandbox = client.create_sandbox(warmpool="python-sandbox-pool")
  >>> sandbox.commands.run("echo 'Hello World'")

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.get_sandbox"></a>

##### get\_sandbox

```python
def get_sandbox(claim_name: str,
                namespace: str = "default",
                resolve_timeout: int = 30) -> T
```

Retrieves an existing sandbox handle given a sandbox claim name.
If the handle is closed or missing, it re-attaches to the infrastructure.
Reattached handles preserve the client's historical automatic cleanup
behavior. Cleanup is constrained to the exact observed Claim UID.

**Arguments**:

- `claim_name` - Name of the SandboxClaim to attach to.
- `namespace` - Kubernetes namespace the claim lives in.
- `resolve_timeout` - Seconds to wait while resolving the sandbox
  name from the claim status.

**Example**:

  
  >>> client = SandboxClient()
  >>> sandbox = client.get_sandbox(
  ...     "sandbox-claim-1234abcd",
  ... )
  >>> sandbox.commands.run("ls -la")

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.list_active_sandboxes"></a>

##### list\_active\_sandboxes

```python
def list_active_sandboxes() -> List[Tuple[str, str]]
```

Returns a list of tuples containing (namespace, claim_name) currently managed by this client.

**Example**:

  
  >>> client = SandboxClient()
  >>> client.create_sandbox("python-sandbox-pool")
  >>> print(client.list_active_sandboxes())
  [('default', 'sandbox-claim-1234abcd')]

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.list_all_sandboxes"></a>

##### list\_all\_sandboxes

```python
def list_all_sandboxes(namespace: str = "default",
                       label_selector: str | None = None) -> List[str]
```

Lists all SandboxClaim names currently existing in the Kubernetes cluster
for the given namespace.

**Arguments**:

- `namespace` - Kubernetes namespace to list claims in.
- `label_selector` - Optional Kubernetes label selector string
  (e.g. ``"app=myapp"``). When set, only claims matching
  the selector are returned.
  

**Example**:

  
  >>> client = SandboxClient()
  >>> print(client.list_all_sandboxes(namespace="default"))
  ['sandbox-claim-1234abcd', 'sandbox-claim-5678efgh']

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.delete_sandbox"></a>

##### delete\_sandbox

```python
def delete_sandbox(claim_name: str, namespace: str = "default")
```

Stops the client side connection and deletes the Kubernetes resources.

**Example**:

  
  >>> client = SandboxClient()
  >>> sandbox = client.create_sandbox("python-sandbox-pool")
  >>> client.delete_sandbox(sandbox.claim_name)

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.delete_all"></a>

##### delete\_all

```python
def delete_all()
```

Deliberately delete every sandbox tracked by this client.

**Example**:

  
  >>> client = SandboxClient()
  >>> client.create_sandbox("python-sandbox-pool")
  >>> client.create_sandbox("python-sandbox-pool")
  >>> client.delete_all()

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.get_sandbox_claim_warmpool_name"></a>

##### get\_sandbox\_claim\_warmpool\_name

```python
def get_sandbox_claim_warmpool_name(claim_name: str, namespace: str) -> str
```

Get warmpool name of a sandbox claim.

<a id="k8s_agent_sandbox.models"></a>

## k8s\_agent\_sandbox.models

<a id="k8s_agent_sandbox.models.ExecutionResult"></a>

### ExecutionResult Objects

```python
class ExecutionResult(BaseModel)
```

A structured object for holding the result of a command execution.

<a id="k8s_agent_sandbox.models.ExecutionResult.stdout"></a>

##### stdout

Standard output from the command.

<a id="k8s_agent_sandbox.models.ExecutionResult.stderr"></a>

##### stderr

Standard error from the command.

<a id="k8s_agent_sandbox.models.ExecutionResult.exit_code"></a>

##### exit\_code

Exit code of the command.

<a id="k8s_agent_sandbox.models.FileEntry"></a>

### FileEntry Objects

```python
class FileEntry(BaseModel)
```

Represents a file or directory entry in the sandbox.

Runtime-neutral: the SDK decodes both the legacy python-runtime wire
format (``mod_time`` as a float POSIX timestamp) and the sandboxd wire
format (``modified_at`` as an RFC 3339 string, plus ``mode``) into this
one shape. ``modified`` is always a timezone-aware datetime.

<a id="k8s_agent_sandbox.models.FileEntry.name"></a>

##### name

Name of the file.

<a id="k8s_agent_sandbox.models.FileEntry.size"></a>

##### size

Size of the file in bytes.

<a id="k8s_agent_sandbox.models.FileEntry.type"></a>

##### type

Type of the entry (file or directory).

<a id="k8s_agent_sandbox.models.FileEntry.modified"></a>

##### modified

Last modification time (timezone-aware).

<a id="k8s_agent_sandbox.models.FileEntry.mode"></a>

##### mode

Octal permission bits (sandboxd only), e.g. "0644".

<a id="k8s_agent_sandbox.models.FileEntry.from_legacy"></a>

##### from\_legacy

```python
@classmethod
def from_legacy(cls, entry: dict) -> "FileEntry"
```

Build from the legacy python-runtime listing entry.

<a id="k8s_agent_sandbox.models.FileEntry.from_sandboxd"></a>

##### from\_sandboxd

```python
@classmethod
def from_sandboxd(cls, entry: dict) -> "FileEntry"
```

Build from a sandboxd DirectoryListing entry.

<a id="k8s_agent_sandbox.models.SandboxClaimEnvVar"></a>

### SandboxClaimEnvVar Objects

```python
class SandboxClaimEnvVar(BaseModel)
```

Represents an environment variable entry in a SandboxClaim spec.

<a id="k8s_agent_sandbox.models.SandboxClaimEnvVar.name"></a>

##### name

Name of the environment variable.

<a id="k8s_agent_sandbox.models.SandboxClaimEnvVar.value"></a>

##### value

Value of the environment variable.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig"></a>

### SandboxDirectConnectionConfig Objects

```python
class SandboxDirectConnectionConfig(BaseModel)
```

Configuration for connecting directly to a Sandbox URL.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.api_url"></a>

##### api\_url

Direct URL to the router.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.server_port"></a>

##### server\_port

Port the sandbox container listens on.

<a id="k8s_agent_sandbox.models.SandboxGatewayConnectionConfig"></a>

### SandboxGatewayConnectionConfig Objects

```python
class SandboxGatewayConnectionConfig(BaseModel)
```

Configuration for connecting via Kubernetes Gateway API.

<a id="k8s_agent_sandbox.models.SandboxGatewayConnectionConfig.gateway_name"></a>

##### gateway\_name

Name of the Gateway resource.

<a id="k8s_agent_sandbox.models.SandboxGatewayConnectionConfig.gateway_namespace"></a>

##### gateway\_namespace

Namespace where the Gateway resource resides.

<a id="k8s_agent_sandbox.models.SandboxGatewayConnectionConfig.gateway_ready_timeout"></a>

##### gateway\_ready\_timeout

Timeout in seconds to wait for Gateway IP.

<a id="k8s_agent_sandbox.models.SandboxGatewayConnectionConfig.server_port"></a>

##### server\_port

Port the sandbox container listens on.

<a id="k8s_agent_sandbox.models.SandboxLocalTunnelConnectionConfig"></a>

### SandboxLocalTunnelConnectionConfig Objects

```python
class SandboxLocalTunnelConnectionConfig(BaseModel)
```

Configuration for connecting via kubectl port-forward.

<a id="k8s_agent_sandbox.models.SandboxLocalTunnelConnectionConfig.port_forward_ready_timeout"></a>

##### port\_forward\_ready\_timeout

Timeout in seconds to wait for port-forward to be ready.

<a id="k8s_agent_sandbox.models.SandboxLocalTunnelConnectionConfig.server_port"></a>

##### server\_port

Port the sandbox container listens on.

<a id="k8s_agent_sandbox.models.SandboxLocalTunnelConnectionConfig.router_namespace"></a>

##### router\_namespace

Namespace where the Router service resides.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig"></a>

### SandboxdPodTunnelConnectionConfig Objects

```python
class SandboxdPodTunnelConnectionConfig(BaseModel)
```

Configuration for the sandboxd runtime via a direct pod port-forward.

sandboxd (KEP-539.2) exposes two listeners: the Filesystem & Runtime REST
API and the gRPC ProcessService. This config port-forwards directly to the
sandbox pod, reaching both.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig.rest_port"></a>

##### rest\_port

sandboxd REST filesystem port on the pod.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig.grpc_port"></a>

##### grpc\_port

sandboxd gRPC ProcessService port on the pod.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig.port_forward_ready_timeout"></a>

##### port\_forward\_ready\_timeout

Seconds to wait for port-forward readiness.

<a id="k8s_agent_sandbox.models.SandboxInClusterConnectionConfig"></a>

### SandboxInClusterConnectionConfig Objects

```python
class SandboxInClusterConnectionConfig(BaseModel)
```

Configuration for direct in-cluster connection to the sandbox pod, bypassing the router.

The client first uses the pod IP reported in the Sandbox status. If the pod IP
is unavailable, it falls back to the stable Kubernetes DNS endpoint:
    http://{sandbox_id}.{namespace}.svc.cluster.local:{server_port}

<a id="k8s_agent_sandbox.models.SandboxInClusterConnectionConfig.server_port"></a>

##### server\_port

Port the sandbox container listens on.

<a id="k8s_agent_sandbox.models.SandboxTracerConfig"></a>

### SandboxTracerConfig Objects

```python
class SandboxTracerConfig(BaseModel)
```

Configuration for tracer level information

<a id="k8s_agent_sandbox.models.SandboxTracerConfig.enable_tracing"></a>

##### enable\_tracing

Whether to enable OpenTelemetry tracing.

<a id="k8s_agent_sandbox.models.SandboxTracerConfig.trace_service_name"></a>

##### trace\_service\_name

Service name used for traces.

