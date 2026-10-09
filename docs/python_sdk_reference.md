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
             cleanup: bool = False,
             api_client: client.ApiClient | None = None) -> None
```

Initializes the SandboxClient.

**Arguments**:

- `connection_config` - Configuration for connecting to the sandboxes.
  Defaults to SandboxLocalTunnelConnectionConfig() which uses
  kubectl port-forwarding. Sandboxd supports a pod tunnel or the
  explicit Service DNS / Pod IP in-cluster connection config.
- `tracer_config` - Configuration for OpenTelemetry tracing.
  Defaults to an empty SandboxTracerConfig (tracing disabled).
- `cleanup` - If True, registers an atexit hook to automatically delete
  tracked sandboxes when the program terminates, excluding claims
  explicitly named through create_sandbox(). Defaults to False.
- `api_client` - Optional pre-configured Kubernetes ``ApiClient`` forwarded
  to the underlying ``K8sHelper`` to target a specific cluster/context.

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
- `claim_name` - Optional DNS-1123 Claim name. Explicit names remain
  caller-owned and are excluded from automatic cleanup.
- `adopt_existing` - On 409, attach to the existing named Claim after
  checking its warm pool and that it is not terminating. Requires
  claim_name. Creation options are not reapplied on adoption;
  an existing shutdownTime is preserved.
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
def delete_sandbox(claim_name: str, namespace: str = "default") -> None
```

Stops the client side connection and deletes the Kubernetes resources.

**Example**:

  
  >>> client = SandboxClient()
  >>> sandbox = client.create_sandbox("python-sandbox-pool")
  >>> client.delete_sandbox(sandbox.claim_name)

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.delete_all"></a>

##### delete\_all

```python
def delete_all() -> None
```

Cleanup all tracked sandboxes managed by this client.

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

<a id="k8s_agent_sandbox.async_sandbox_client"></a>

## k8s\_agent\_sandbox.async\_sandbox\_client

Async version of :class:`SandboxClient` for use in async applications.

Requires the ``async`` optional dependencies::

    pip install k8s-agent-sandbox[async]

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient"></a>

### AsyncSandboxClient Objects

```python
class AsyncSandboxClient(Generic[T])
```

Async registry-based client for managing Sandbox lifecycles.

Use as an async context manager for automatic cleanup::

    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox("python-sandbox-pool")
        result = await sandbox.commands.run("echo hello")

``connection_config`` is required — the async client does not support
``SandboxLocalTunnelConnectionConfig``.

By default (``cleanup=True``) an atexit hook is registered that deletes
tracked sandboxes on program termination, except explicitly named Claims.
The hook also terminates
loop-independent local resources such as sandboxd port-forward processes.
Pass ``cleanup=False`` to opt out of this behavior::

    client = AsyncSandboxClient(connection_config=config, cleanup=False)

Note that this default differs from the synchronous ``SandboxClient``,
which defaults to ``cleanup=False``; the async client opts in to safer
out-of-the-box cleanup.

Use ``async with`` to delete automatically managed Claims and close local
connections. Explicitly named Claims remain caller-owned and are not
deleted on context exit. To delete them, explicitly call
``await client.delete_sandbox(...)`` or ``await client.delete_all()``
before closing the client. Outside a context manager, call
``await client.close()`` to close connections and the Kubernetes API client.

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.sandbox_class"></a>

##### sandbox\_class

type: ignore

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.__init__"></a>

##### \_\_init\_\_

```python
def __init__(connection_config: SandboxConnectionConfig | None = None,
             tracer_config: SandboxTracerConfig | None = None,
             cleanup: bool = True,
             api_client: async_client.ApiClient | None = None) -> None
```

**Arguments**:

- `connection_config` - Configuration for connecting to the sandboxes.
  Required — the async client does not support
  ``SandboxLocalTunnelConnectionConfig``.
- `tracer_config` - Configuration for OpenTelemetry tracing.
  Defaults to an empty SandboxTracerConfig (tracing disabled).
- `cleanup` - If True, registers an atexit hook to automatically delete
  tracked sandboxes when the program terminates, excluding claims
  explicitly named through create_sandbox(). The hook
  synchronously terminates loop-independent local resources and
  uses the synchronous ``K8sHelper`` for claim deletion, so it
  remains usable during interpreter shutdown. Cleanup is
  best-effort — per-claim and top-level failures emit warnings to
  ``sys.stderr`` rather than raising. Defaults to True so that
  sandboxes are not leaked when a caller forgets to clean up;
  pass ``cleanup=False`` to opt out. Note this differs from the
  synchronous ``SandboxClient``, which defaults to False.
- `api_client` - Optional pre-configured ``kubernetes_asyncio`` ``ApiClient``
  forwarded to the underlying ``AsyncK8sHelper`` to target a specific
  cluster/context.

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.close"></a>

##### close

```python
async def close() -> None
```

Shuts down tracked sandbox connections and the K8s API client.

A connection that fails to close remains tracked so a later call can
retry its cleanup.

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.create_sandbox"></a>

##### create\_sandbox

```python
async def create_sandbox(warmpool: str,
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

Provisions a new Sandbox claim and returns an async Sandbox handle.

**Arguments**:

- `warmpool` - Name of the SandboxWarmPool to use.
- `namespace` - Kubernetes namespace for the claim.
- `sandbox_ready_timeout` - Seconds to wait for the sandbox to be ready.
- `labels` - Optional Kubernetes labels to attach to the claim object
  (``SandboxClaim.metadata.labels``).
- `claim_name` - Optional DNS-1123 Claim name. Explicit names remain
  caller-owned and are excluded from automatic cleanup.
- `adopt_existing` - On 409, attach to the existing named Claim after
  checking its warm pool and that it is not terminating. Requires
  claim_name. Creation options are not reapplied on adoption;
  an existing shutdownTime is preserved.
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
  
  Example::
  
  async with AsyncSandboxClient(connection_config=config) as client:
  sandbox = await client.create_sandbox("python-sandbox-pool")
  result = await sandbox.commands.run("echo 'Hello'")

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.get_sandbox"></a>

##### get\_sandbox

```python
async def get_sandbox(claim_name: str,
                      namespace: str = "default",
                      resolve_timeout: int = 30,
                      warmpool_name: str | None = None) -> T
```

Retrieves an existing sandbox handle given a sandbox claim name.

**Arguments**:

- `claim_name` - Name of the SandboxClaim to attach to.
- `namespace` - Kubernetes namespace the claim lives in.
- `resolve_timeout` - Seconds to wait while resolving the sandbox
  name from the claim status.
- `warmpool_name` - Optional SandboxWarmPool name to validate against
  the existing claim's ``spec.warmPoolRef.name``.
  When supplied and the claim references a different
  warmpool, ``ValueError`` is raised before returning a
  handle. Mirrors the sync ``SandboxClient.get_sandbox``
  guard so async session-reattach callers get the same
  refuse-on-mismatch semantics.
  
  Example::
  
  sandbox = await client.get_sandbox("sandbox-claim-1234abcd")
  result = await sandbox.commands.run("ls -la")

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.list_active_sandboxes"></a>

##### list\_active\_sandboxes

```python
async def list_active_sandboxes() -> list[tuple[str, str]]
```

Returns a list of ``(namespace, claim_name)`` tuples currently managed.

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.list_all_sandboxes"></a>

##### list\_all\_sandboxes

```python
async def list_all_sandboxes(namespace: str = "default",
                             label_selector: str | None = None) -> list[str]
```

Lists all SandboxClaim names in the Kubernetes cluster for a namespace.

**Arguments**:

- `namespace` - Kubernetes namespace to list claims in.
- `label_selector` - Optional Kubernetes label selector string
  (e.g. ``"app=myapp"``). When set, only claims matching
  the selector are returned.

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.delete_sandbox"></a>

##### delete\_sandbox

```python
async def delete_sandbox(claim_name: str, namespace: str = "default") -> None
```

Stops the client side connection and deletes the Kubernetes resources.

<a id="k8s_agent_sandbox.async_sandbox_client.AsyncSandboxClient.delete_all"></a>

##### delete\_all

```python
async def delete_all() -> None
```

Cleanup all tracked sandboxes managed by this client.

<a id="k8s_agent_sandbox.sandbox"></a>

## k8s\_agent\_sandbox.sandbox

<a id="k8s_agent_sandbox.sandbox.Sandbox"></a>

### Sandbox Objects

```python
class Sandbox()
```

Represents a connection to a specific running Sandbox instance.

This class provides the interface for interacting with the Sandbox, including:
- Executing commands via the `commands` property.
- Managing files via the `files` property.
- Handling the underlying connection.
- Integrating with OpenTelemetry for tracing operations.

<a id="k8s_agent_sandbox.sandbox.Sandbox.get_pod_name"></a>

##### get\_pod\_name

```python
def get_pod_name() -> str
```

Fetches the Sandbox object from Kubernetes and retrieves its current pod name.

<a id="k8s_agent_sandbox.sandbox.Sandbox.get_sandbox_name_hash"></a>

##### get\_sandbox\_name\_hash

```python
def get_sandbox_name_hash() -> str | None
```

Fetches the Sandbox object from Kubernetes and retrieves its name hash from selector.
Caches the result to avoid repeated API calls.

<a id="k8s_agent_sandbox.sandbox.Sandbox.get_pod_ip"></a>

##### get\_pod\_ip

```python
def get_pod_ip() -> str | None
```

Selects a pod IP from the Sandbox status (prefers IPv4, normalizes canonical form).

Always queries the K8s API for the latest IP — the pod IP can change
after a pod restart (e.g. when spec.operatingMode is set to Suspended and resumed
via setting spec.operatingMode to Running).
Returns None if no valid IP can be selected.

<a id="k8s_agent_sandbox.sandbox.Sandbox.get_service_fqdn"></a>

##### get\_service\_fqdn

```python
def get_service_fqdn() -> str | None
```

Return the controller-reported Service FQDN, when available.

<a id="k8s_agent_sandbox.sandbox.Sandbox.status"></a>

##### status

```python
def status() -> tuple[str, str]
```

Retrieves the current status of the Sandbox by inspecting its Kubernetes conditions.

Returns a tuple of (status, message).
status can be 'SandboxReady', 'SandboxNotFound', or 'SandboxNotReady'.
message contains the Kubernetes condition message if available.

<a id="k8s_agent_sandbox.sandbox.Sandbox.is_active"></a>

##### is\_active

```python
@property
def is_active() -> bool
```

Returns True if the connection hasn't been explicitly closed 
and engines are still initialized.

<a id="k8s_agent_sandbox.sandbox.Sandbox.close_connection"></a>

##### close\_connection

```python
def close_connection() -> None
```

Closes the client-side connection and disables execution engines locally,
but leaves the remote Kubernetes Sandbox infrastructure running.

Use this to free up local resources (like port-forwards or HTTP sessions).

<a id="k8s_agent_sandbox.sandbox.Sandbox.terminate"></a>

##### terminate

```python
def terminate()
```

Permanent deletion of all server side infrastructure and client side connection.

This method is idempotent. After a successful delete, ``claim_name`` is
cleared so later calls are a local no-op and do not issue another DELETE.
If the claim is already gone remotely, ``delete_sandbox_claim`` treats a
404 as success rather than raising.

<a id="k8s_agent_sandbox.async_sandbox"></a>

## k8s\_agent\_sandbox.async\_sandbox

Async handle for one running Sandbox and its local client resources.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox"></a>

### AsyncSandbox Objects

```python
class AsyncSandbox()
```

Represents an async connection to a specific running Sandbox instance.

This class provides the async interface for interacting with the Sandbox:
- Executing commands via the ``commands`` property.
- Managing files via the ``files`` property.
- Handling the underlying connection lifecycle.
- Integrating with OpenTelemetry for tracing operations.

Unlike the sync ``Sandbox``, ``connection_config`` is required because the
async client does not support ``SandboxLocalTunnelConnectionConfig``.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.get_pod_name"></a>

##### get\_pod\_name

```python
async def get_pod_name() -> str
```

Fetches the Sandbox object from Kubernetes and retrieves its current pod name.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.get_sandbox_name_hash"></a>

##### get\_sandbox\_name\_hash

```python
async def get_sandbox_name_hash() -> str | None
```

Fetches the Sandbox object from Kubernetes and retrieves its name hash from selector.
Caches the result to avoid repeated API calls.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.get_pod_ip"></a>

##### get\_pod\_ip

```python
async def get_pod_ip() -> str | None
```

Selects a pod IP from the Sandbox status (prefers IPv4, normalizes canonical form).

Always queries the K8s API for the latest IP — the pod IP can change
after a pod restart (e.g. when spec.operatingMode is set to Suspended and resumed
via setting spec.operatingMode to Running).
Returns None if no valid IP can be selected.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.get_service_fqdn"></a>

##### get\_service\_fqdn

```python
async def get_service_fqdn() -> str | None
```

Return the controller-reported Service FQDN, when available.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.status"></a>

##### status

```python
async def status() -> tuple[str, str]
```

Retrieves the current status of the Sandbox by inspecting its Kubernetes conditions.

Returns a tuple of (status, message).
status can be 'SandboxReady', 'SandboxNotFound', or 'SandboxNotReady'.
message contains the Kubernetes condition message if available.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.commands"></a>

##### commands

```python
@property
def commands() -> AsyncCommandExecutor | None
```

Return the command client while this handle is active.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.files"></a>

##### files

```python
@property
def files() -> AsyncFilesystem | None
```

Return the filesystem client while this handle is active.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.is_active"></a>

##### is\_active

```python
@property
def is_active() -> bool
```

Returns True if the connection hasn't been explicitly closed
and engines are still initialized.

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.close_connection"></a>

##### close\_connection

```python
async def close_connection() -> None
```

Closes the client-side connection and disables execution engines locally,
but leaves the remote Kubernetes Sandbox infrastructure running.

Use this to free up local resources (like port-forwards or HTTP sessions).

<a id="k8s_agent_sandbox.async_sandbox.AsyncSandbox.terminate"></a>

##### terminate

```python
async def terminate() -> None
```

Permanent deletion of all server side infrastructure and client side connection.

This method is idempotent. After a successful delete, ``claim_name`` is
cleared so later calls are a local no-op and do not issue another DELETE.
If the claim is already gone remotely, ``delete_sandbox_claim`` treats a
404 as success rather than raising.

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

<a id="k8s_agent_sandbox.models.ExecutionResult.timed_out"></a>

##### timed\_out

True if the runtime killed the command for exceeding its time limit.

<a id="k8s_agent_sandbox.models.LegacyExecuteRequest"></a>

### LegacyExecuteRequest Objects

```python
class LegacyExecuteRequest(BaseModel)
```

Request body for the legacy python-runtime /execute endpoint.

<a id="k8s_agent_sandbox.models.LegacyExecuteRequest.command"></a>

##### command

Shell command to run.

<a id="k8s_agent_sandbox.models.LegacyExecuteRequest.timeout_seconds"></a>

##### timeout\_seconds

Limit on how long the command may run; omitted when unset.

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

``extra_headers`` and ``client_cert``/``ca_cert`` (mTLS) support a router
behind an authenticating gateway.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.api_url"></a>

##### api\_url

Direct URL to the router.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.server_port"></a>

##### server\_port

Port the sandbox container listens on.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.extra_headers"></a>

##### extra\_headers

Sent on every request.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.client_cert"></a>

##### client\_cert

(certificate path, private key path) for mTLS.

<a id="k8s_agent_sandbox.models.SandboxDirectConnectionConfig.ca_cert"></a>

##### ca\_cert

CA bundle path used to verify the router; default trust store if unset.

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

Doesn't work on Kata or gVisor; use SandboxdInClusterConnectionConfig for those.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig.rest_port"></a>

##### rest\_port

sandboxd REST filesystem port on the pod.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig.grpc_port"></a>

##### grpc\_port

sandboxd gRPC ProcessService port on the pod.

<a id="k8s_agent_sandbox.models.SandboxdPodTunnelConnectionConfig.port_forward_ready_timeout"></a>

##### port\_forward\_ready\_timeout

Seconds to wait for port-forward readiness.

<a id="k8s_agent_sandbox.models.SandboxdInClusterConnectionConfig"></a>

### SandboxdInClusterConnectionConfig Objects

```python
class SandboxdInClusterConnectionConfig(BaseModel)
```

Connect to sandboxd directly over the selected in-cluster address.

``in-cluster-service`` requires ``Sandbox.status.serviceFQDN`` and a Service
enabled on the Sandbox template. ``in-cluster-pod-ip`` uses
``Sandbox.status.podIPs``. Neither mode falls back to the other. sandboxd's
REST filesystem listener defaults to port 8080 and its gRPC ProcessService
listener to port 9090.

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

<a id="k8s_agent_sandbox.exceptions"></a>

## k8s\_agent\_sandbox.exceptions

<a id="k8s_agent_sandbox.exceptions.SandboxError"></a>

### SandboxError Objects

```python
class SandboxError(RuntimeError)
```

Base class for all sandbox-related errors.

<a id="k8s_agent_sandbox.exceptions.SandboxNotReadyError"></a>

### SandboxNotReadyError Objects

```python
class SandboxNotReadyError(SandboxError)
```

Raised when the sandbox is not ready for communication.

<a id="k8s_agent_sandbox.exceptions.SandboxNoServiceError"></a>

### SandboxNoServiceError Objects

```python
class SandboxNoServiceError(SandboxError)
```

Raised in ``in-cluster-service`` mode if the Sandbox has no Service FQDN.

<a id="k8s_agent_sandbox.exceptions.SandboxNotFoundError"></a>

### SandboxNotFoundError Objects

```python
class SandboxNotFoundError(SandboxError)
```

Raised when the sandbox or sandbox claim cannot be found or was deleted.

<a id="k8s_agent_sandbox.exceptions.SnapshotNotFoundError"></a>

### SnapshotNotFoundError Objects

```python
class SnapshotNotFoundError(SandboxError)
```

Raised when the requested snapshot does not exist.

<a id="k8s_agent_sandbox.exceptions.SandboxTemplateNotFoundError"></a>

### SandboxTemplateNotFoundError Objects

```python
class SandboxTemplateNotFoundError(SandboxError)
```

Raised when the requested sandbox template does not exist.

<a id="k8s_agent_sandbox.exceptions.SandboxWarmPoolNotFoundError"></a>

### SandboxWarmPoolNotFoundError Objects

```python
class SandboxWarmPoolNotFoundError(SandboxError)
```

Raised when the requested sandbox warm pool does not exist.

<a id="k8s_agent_sandbox.exceptions.SandboxPortForwardError"></a>

### SandboxPortForwardError Objects

```python
class SandboxPortForwardError(SandboxError)
```

Raised when the port-forward process crashes.

<a id="k8s_agent_sandbox.exceptions.SandboxMetadataError"></a>

### SandboxMetadataError Objects

```python
class SandboxMetadataError(SandboxError)
```

Raised when the sandbox object is missing expected metadata.

<a id="k8s_agent_sandbox.exceptions.SandboxRequestError"></a>

### SandboxRequestError Objects

```python
class SandboxRequestError(SandboxError)
```

Raised when an HTTP request to the sandbox fails.

**Attributes**:

- `status_code` - The HTTP status code, if available.
- `response` - The raw response object (``requests.Response`` or
  ``httpx.Response``), if available.

<a id="k8s_agent_sandbox.exceptions.SandboxClaimFailedError"></a>

### SandboxClaimFailedError Objects

```python
class SandboxClaimFailedError(SandboxError)
```

The SandboxClaim reported a terminal Ready=False reason.

The claim controller will not retry these (e.g. InvalidMetadata,
VolumeClaimTemplatesError, ClaimExpired); the claim will not become
ready without user action, so ready-waits raise instead of waiting
out the timeout.

<a id="k8s_agent_sandbox.commands.command_executor"></a>

## k8s\_agent\_sandbox.commands.command\_executor

<a id="k8s_agent_sandbox.commands.command_executor.CommandExecutor"></a>

### CommandExecutor Objects

```python
class CommandExecutor()
```

Handles execution of commands within the sandbox.

<a id="k8s_agent_sandbox.commands.command_executor.CommandExecutor.run"></a>

##### run

```python
@trace_span("run")
def run(command: str,
        timeout: int = 60,
        command_timeout: float | None = None) -> ExecutionResult
```

Run a shell command and return its output and exit code.

**Arguments**:

- `command` - The shell command to run in the sandbox.
- `timeout` - Seconds to wait for the sandbox to respond.
- `command_timeout` - Optional limit, in seconds, on how long the command
  itself may run. The sandbox kills the command when it is
  exceeded, and the read timeout is extended past it so the
  result still arrives. The legacy runtime reports this as an
  ExecutionResult with ``timed_out`` set (runtimes that predate
  the field ignore the limit). sandboxd uses it as the gRPC
  deadline and reports an exceeded deadline the same way, with
  exit code 124 and ``timed_out`` set; output written before the
  deadline is not returned. Through the sandbox router a request
  is also bounded by the router's ``--proxy-timeout`` (180s by
  default), so a longer ``command_timeout`` ends in a 502/504
  from the router instead of a ``timed_out`` result.

<a id="k8s_agent_sandbox.commands.async_command_executor"></a>

## k8s\_agent\_sandbox.commands.async\_command\_executor

<a id="k8s_agent_sandbox.commands.async_command_executor.AsyncCommandExecutor"></a>

### AsyncCommandExecutor Objects

```python
class AsyncCommandExecutor()
```

Run commands through legacy HTTP or sandboxd's gRPC service.

<a id="k8s_agent_sandbox.commands.async_command_executor.AsyncCommandExecutor.run"></a>

##### run

```python
@async_trace_span("run")
async def run(command: str,
              timeout: int = 60,
              command_timeout: float | None = None) -> ExecutionResult
```

Run a shell command and return its output and exit code.

**Arguments**:

- `command` - The shell command to run in the sandbox.
- `timeout` - Seconds to wait for the sandbox to respond.
- `command_timeout` - Optional limit, in seconds, on how long the command
  itself may run. The sandbox kills the command when it is
  exceeded, and the read timeout is extended past it so the
  result still arrives. The legacy runtime reports this as an
  ExecutionResult with ``timed_out`` set (runtimes that predate
  the field ignore the limit). sandboxd uses it as the gRPC
  deadline and reports an exceeded deadline the same way, with
  exit code 124 and ``timed_out`` set; output written before the
  deadline is not returned. Through the sandbox router a request
  is also bounded by the router's ``--proxy-timeout`` (180s by
  default), so a longer ``command_timeout`` ends in a 502/504
  from the router instead of a ``timed_out`` result.

<a id="k8s_agent_sandbox.files.filesystem"></a>

## k8s\_agent\_sandbox.files.filesystem

Synchronous filesystem operations for legacy and sandboxd runtimes.

<a id="k8s_agent_sandbox.files.filesystem.BinaryWriter"></a>

### BinaryWriter Objects

```python
class BinaryWriter(Protocol)
```

A synchronous destination that accepts binary file content.

<a id="k8s_agent_sandbox.files.filesystem.BinaryWriter.write"></a>

##### write

```python
def write(content: bytes) -> int
```

Write content and return the number of accepted bytes.

<a id="k8s_agent_sandbox.files.filesystem.Filesystem"></a>

### Filesystem Objects

```python
class Filesystem()
```

Handles file operations within the sandbox.

Speaks either the legacy python-runtime HTTP API or the sandboxd
Filesystem & Runtime REST API, selected by the connection config
(``connector.is_sandboxd()``).

<a id="k8s_agent_sandbox.files.filesystem.Filesystem.write"></a>

##### write

```python
@trace_span("write")
def write(path: str,
          content: bytes | str | BinaryIO,
          timeout: int = 60,
          allow_unsafe_paths: bool = False) -> None
```

Upload content to a sandbox-relative path.

Binary file objects are read in chunks from their current position and
sent once without retries. The caller must close the file object; its
position may advance if the upload fails.

<a id="k8s_agent_sandbox.files.filesystem.Filesystem.read"></a>

##### read

```python
@trace_span("read")
def read(path: str,
         timeout: int = 60,
         allow_unsafe_paths: bool = False) -> bytes
```

Read a sandbox-relative file and return its raw bytes.

<a id="k8s_agent_sandbox.files.filesystem.Filesystem.read_to"></a>

##### read\_to

```python
@trace_span("read_to")
def read_to(path: str,
            destination: BinaryWriter,
            timeout: int = 60,
            allow_unsafe_paths: bool = False,
            max_bytes: int | None = None) -> int
```

Stream a sandbox file into a caller-owned binary destination.

The destination is never closed. If ``max_bytes`` is set, at most that
many bytes are written before an oversized download raises
``RuntimeError``. Data written before an error remains in the
destination. The returned value is the number of bytes written.

<a id="k8s_agent_sandbox.files.filesystem.Filesystem.list"></a>

##### list

```python
@trace_span("list")
def list(path: str, timeout: int = 60) -> List[FileEntry]
```

List files and directories at a sandbox-relative path.

<a id="k8s_agent_sandbox.files.filesystem.Filesystem.exists"></a>

##### exists

```python
@trace_span("exists")
def exists(path: str, timeout: int = 60) -> bool
```

Return whether a path exists without downloading its contents.

<a id="k8s_agent_sandbox.files.filesystem.Filesystem.delete"></a>

##### delete

```python
@trace_span("delete")
def delete(path: str, recursive: bool = False, timeout: int = 60) -> None
```

Remove a file or directory. sandboxd runtime only.

With ``recursive=True`` directories are removed with their contents;
otherwise deleting a non-empty directory fails with a 409. The legacy
python-runtime has no delete endpoint and raises NotImplementedError.

<a id="k8s_agent_sandbox.files.async_filesystem"></a>

## k8s\_agent\_sandbox.files.async\_filesystem

Asynchronous filesystem operations for sandbox runtimes.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncBinaryWriter"></a>

### AsyncBinaryWriter Objects

```python
class AsyncBinaryWriter(Protocol)
```

An asynchronous destination that accepts binary file content.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncBinaryWriter.write"></a>

##### write

```python
def write(content: bytes) -> Awaitable[int]
```

Write content and return the number of accepted bytes.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem"></a>

### AsyncFilesystem Objects

```python
class AsyncFilesystem()
```

Read and modify sandbox files without blocking the event loop.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem.write"></a>

##### write

```python
@async_trace_span("write")
async def write(path: str,
                content: bytes | str | BinaryIO,
                timeout: int = 60,
                allow_unsafe_paths: bool = False) -> None
```

Upload content to a sandbox-relative path.

Binary file objects are read in chunks from their current position and
sent once without retries. The caller must close the file object; its
position may advance if the upload fails.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem.read"></a>

##### read

```python
@async_trace_span("read")
async def read(path: str,
               timeout: int = 60,
               allow_unsafe_paths: bool = False) -> bytes
```

Read a sandbox-relative file and return its raw bytes.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem.read_to"></a>

##### read\_to

```python
@async_trace_span("read_to")
async def read_to(path: str,
                  destination: AsyncBinaryWriter,
                  timeout: int = 60,
                  allow_unsafe_paths: bool = False,
                  max_bytes: int | None = None) -> int
```

Stream a sandbox file into a caller-owned asynchronous destination.

The destination is never closed. If ``max_bytes`` is set, at most that
many bytes are written before an oversized download raises
``RuntimeError``. Data written before an error or cancellation remains
in the destination. The returned value is the number of bytes written.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem.list"></a>

##### list

```python
@async_trace_span("list")
async def list(path: str, timeout: int = 60) -> list[FileEntry]
```

List files and directories at a sandbox-relative path.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem.exists"></a>

##### exists

```python
@async_trace_span("exists")
async def exists(path: str, timeout: int = 60) -> bool
```

Return whether a path exists without downloading its contents.

<a id="k8s_agent_sandbox.files.async_filesystem.AsyncFilesystem.delete"></a>

##### delete

```python
@async_trace_span("delete")
async def delete(path: str,
                 recursive: bool = False,
                 timeout: int = 60) -> None
```

Delete a sandboxd path, optionally including directory contents.

The legacy runtime has no delete endpoint and raises
``NotImplementedError``.

