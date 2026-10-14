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
  explicitly named through create_sandbox(), and to release tracked
  batches. Defaults to False.
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
                   volume_claim_templates: list[dict[str, Any]] | None = None,
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

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.claim_batch"></a>

##### claim\_batch

```python
def claim_batch(groups: Sequence[BatchGroup],
                *,
                namespace: str = "default",
                labels: dict[str, str] | None = None,
                batch_id: str | None = None,
                create_rps: float | None = None,
                max_in_flight: int | None = None,
                work_budget: int | None = None,
                quorum_timeout: int | None = None,
                lease_duration: int | None = None) -> SandboxBatch
```

Claims a batch of sandboxes across one or more warm pools, returning its handle at once.

Checks that every group's ``SandboxWarmPool`` and its ``SandboxTemplate`` exist, creates the
batch Lease ``batch-<id>``, and starts the batch's watch. The claims ``<id>-0`` to
``<id>-<N-1>``, where ``N`` is the sum of the group sizes, are then created in the background
until ``err()`` is set. Each group's claims get consecutive ordinals in group order. Each
group's first ``min_ready`` claims are created first, one group after another in group
order, and the rest follow in the same order. Use ``events()`` or ``iter_ready_groups()``
to consume them as they become Ready, and ``release()`` to delete the batch.

Every claim gets the same ``shutdownTime``, the time of this call plus ``N / create_rps``
seconds (rounded up) plus ``quorum_timeout`` plus ``work_budget`` plus 600 seconds, so an
abandoned batch is eventually deleted by the controller. ``work_budget`` is a hard cap.
Nothing extends a claim's ``shutdownTime``, so a batch held for a whole run sets
``work_budget`` to the run's length.

Of the errors below, only ``BatchExistsError`` is a ``BatchError``. The others mean a bad
argument, a missing pool or template, or an API error. Failures after this call returns,
such as a failed create, aren't raised here but show in ``members()`` and ``err()``.

**Arguments**:

- `groups` - One ``BatchGroup`` per warm pool. A group with ``size`` 0 creates no claims
  and gets its members from ``acquire()``, and the quorum consumers skip it.
- `namespace` - Kubernetes namespace for the claims and the Lease.
- `labels` - Optional labels for every claim. They are also set on the Lease, so a handle
  from ``get_batch`` puts them on the claims it adds with ``acquire()``.
- `batch_id` - Optional batch id; a DNS-1123 label starting with a letter, at most 52
  characters. Generated if omitted.
- `create_rps` - Maximum claim creates started per second. Defaults to 50.
- `max_in_flight` - Maximum claim creates in flight at once. Defaults to 20.
- `work_budget` - Seconds the caller expects to work with the batch after it is Ready;
  part of each claim's ``shutdownTime``. Defaults to 3600.
- `quorum_timeout` - Seconds after the last claim create that a group may take to reach
  ``min_ready``. Defaults to 600.
- `lease_duration` - Seconds the batch Lease stays valid without renewal. Defaults to 60.
  

**Raises**:

- `ValueError` - If an argument is invalid.
- `SandboxWarmPoolNotFoundError` - If a group's warm pool doesn't exist.
- `SandboxTemplateNotFoundError` - If a warm pool's template doesn't exist, or it names none.
- `BatchExistsError` - If a Lease or claims with this batch id already exist.
- `ApiException` - For any other API error before the first claim is created, such as a
  403. If the Lease was already created, it is deleted first.
  

**Example**:

  
  >>> client = SandboxClient()
  >>> batch = client.claim_batch([BatchGroup(warmpool="python-sandbox-pool", size=4, min_ready=2)])
  >>> for group in batch.iter_ready_groups():
  ...     if group.error is None:
  ...         batch.connect(group.members[0]).commands.run("echo hello")
  >>> batch.release()

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.get_batch"></a>

##### get\_batch

```python
def get_batch(batch_id: str, namespace: str = "default") -> SandboxBatch
```

Attaches to an existing batch, taking over its Lease.

Resumes batch lease renewal and starts a label-scoped watch that keeps
``members()`` up to date. Only a batch released with ``detach()`` can be re-attached,
within its grace window. A group whose claims are all gone is unknown to the new handle,
so it isn't in ``groups`` and ``iter_ready_groups()`` doesn't yield it.

The batch errors below are all ``BatchError``, so ``except BatchError`` handles a batch that
can't be attached. ``ValueError`` means a bad batch id, ``ApiException`` an API error,
and a failed request raises the transport's own error.

**Raises**:

- `ValueError` - If ``batch_id`` is not a valid batch id.
- `BatchNotFoundError` - If neither the batch's Lease nor any of its claims exist.
- `BatchLeaseExpiredError` - If the Lease is missing while claims exist, or is stale,
  including after the previous holder crashed.
- `BatchInUseError` - If a live Lease is held by another handle, or another client
  takes it over while attaching.
- `BatchError` - If the Lease's or the claims' batch annotations are invalid.
- `ApiException` - For any other API error such as a 403.
- `urllib3.exceptions.HTTPError` - If a request fails, such as on a connection error or
  timeout.
  

**Example**:

  
  >>> from k8s_agent_sandbox import MemberState
  >>> client = SandboxClient()
  >>> batch = client.get_batch("b1234abcd12")
  >>> ready = [m for m in batch.members() if m.state is MemberState.READY]

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

Cleanup all tracked sandboxes managed by this client, and release every tracked batch.

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
def from_legacy(cls, entry: dict[str, Any]) -> "FileEntry"
```

Build from the legacy python-runtime listing entry.

<a id="k8s_agent_sandbox.models.FileEntry.from_sandboxd"></a>

##### from\_sandboxd

```python
@classmethod
def from_sandboxd(cls, entry: dict[str, Any]) -> "FileEntry"
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

<a id="k8s_agent_sandbox.models.AdditionalPodMetadata"></a>

### AdditionalPodMetadata Objects

```python
class AdditionalPodMetadata(BaseModel)
```

Wire-format for SandboxClaim ``spec.additionalPodMetadata``.

Labels and annotations stamped onto the running Sandbox Pod (not the
SandboxClaim object). Omit empty sections so the manifest never carries
an empty ``additionalPodMetadata``.

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

<a id="k8s_agent_sandbox.models.BatchGroup"></a>

### BatchGroup Objects

```python
class BatchGroup(BaseModel)
```

Represents one warmpool in a batch for multi-pool batch claiming.

<a id="k8s_agent_sandbox.models.BatchGroup.min_ready"></a>

##### min\_ready

Defaults to ``size``.

<a id="k8s_agent_sandbox.models.MemberState"></a>

### MemberState Objects

```python
class MemberState(StrEnum)
```

Where a batch member's claim is in its lifecycle.

``FAILED`` is terminal, so the claim will never become Ready. A member stays ``FAILED`` if its
claim is deleted afterwards, so its state keeps saying why it can't become Ready. ``LOST``
means the claim was deleted before it failed.

<a id="k8s_agent_sandbox.models.Member"></a>

### Member Objects

```python
class Member(BaseModel)
```

Represents the identity of a single claim in a batch.

<a id="k8s_agent_sandbox.models.Member.warmpool"></a>

##### warmpool

The group this member belongs to.

<a id="k8s_agent_sandbox.models.BatchEventType"></a>

### BatchEventType Objects

```python
class BatchEventType(StrEnum)
```

The kind of change a ``BatchEvent`` reports.

<a id="k8s_agent_sandbox.models.BatchEvent"></a>

### BatchEvent Objects

```python
class BatchEvent(BaseModel)
```

One change yielded by ``SandboxBatch.events()``.

<a id="k8s_agent_sandbox.models.GroupReady"></a>

### GroupReady Objects

```python
@dataclass(frozen=True)
class GroupReady()
```

One group's quorum outcome, yielded by ``SandboxBatch.iter_ready_groups()``.

On success, ``members`` holds exactly ``min_ready`` Ready members and ``error`` is ``None``.
On failure, ``error`` is a ``QuorumUnreachableError`` or ``BatchTimeoutError`` and ``members`` is empty.

This is a dataclass rather than a Pydantic model because ``error`` holds a raw Python
``Exception`` object (preserving traceback and causes) rather than serializable data.

<a id="k8s_agent_sandbox.sandbox_batch"></a>

## k8s\_agent\_sandbox.sandbox\_batch

Sync handle for using a claimed or re-attached sandbox batch.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch"></a>

### SandboxBatch Objects

```python
class SandboxBatch()
```

A handle to a batch's claims, obtained via ``SandboxClient.claim_batch`` or
``SandboxClient.get_batch``.

Keeps a live cache of the batch's claims through one label-scoped watch
(a background daemon thread), renews the batch Lease on another daemon
thread, and exposes methods for connecting to ready sandboxes and detaching from the batch.
A claimed batch also creates its claims on background worker threads.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.groups"></a>

##### groups

```python
@property
def groups() -> list[BatchGroup]
```

The batch's ``BatchGroup``\ s; one per warm pool.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.size"></a>

##### size

```python
@property
def size() -> int
```

The total number of claims across all groups.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.members"></a>

##### members

```python
def members(warmpool: str | None = None) -> list[Member]
```

Returns a snapshot of the batch's members, optionally filtered to one warm pool.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.connect"></a>

##### connect

```python
def connect(member: Member) -> "Sandbox"
```

Returns a connected ``Sandbox`` for a ready member. If this handle is already connected
to the Sandbox, it reuses the existing connection, otherwise it creates a new one and caches it for future calls.

Raises ``SandboxNotReadyError`` if the member isn't ready, even while its connection is
cached, and ``BatchError`` once ``err()`` is set, since the Lease may be lost and the cache
can no longer vouch for the member.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.err"></a>

##### err

```python
def err() -> Exception | None
```

Returns the error that stopped the batch's watch or background
Lease renewal (e.g., ``BatchLeaseExpiredError``), or ``None`` while healthy.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.lease_degraded"></a>

##### lease\_degraded

```python
def lease_degraded() -> bool
```

Returns whether background Lease renewal is failing.

``True`` from the first failed renewal until a renewal succeeds. It stays ``True`` once
renewal stops on an error, such as the Lease expiring, so it can be polled at any time.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.events"></a>

##### events

```python
def events() -> Iterator[BatchEvent]
```

Returns an iterator over the batch's events.

``MEMBER_READY`` is yielded once per initial claim that becomes Ready, and ``MEMBER_FAILED``
and ``MEMBER_LOST`` once per initial claim that fails or is deleted. Called on its own, it
streams every Ready member. Called after ``iter_ready_groups()``, it yields a group's members
only after that group has yielded successfully there, so a group that fails yields none.
Called after ``wait_for_quorum()``, it yields the members the quorum doesn't return once the
quorum is reached, and none if the quorum fails. ``iter_ready_groups()`` and
``wait_for_quorum()`` can't be called after ``events()``, since the first consumer to be
called fixes how members are handed out.

The iterator ends once every initial claim is Ready or can't become Ready, once the fill
deadline (``quorum_timeout`` after the last claim create) has passed, or once ``err()`` is
set. A change after that, such as a handed-out member that fails, is yielded only by a later
call of ``events()``, which continues where the previous iterator stopped. ``members()``
always shows each claim's current state.

**Raises**:

- `BatchError` - If this handle has been detached or released, when called or while waiting.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.iter_ready_groups"></a>

##### iter\_ready\_groups

```python
def iter_ready_groups() -> Iterator[GroupReady]
```

Returns an iterator that yields one ``GroupReady`` per group, as soon as its outcome is known.

A group yields its first ``min_ready`` Ready members, in the order they became Ready. It
fails with ``QuorumUnreachableError`` once too few of its members can still become Ready,
or with ``BatchTimeoutError`` if it hasn't reached ``min_ready`` within the quorum timeout.
The iterator ends once every group has yielded, or once ``err()`` is set. Calling it again
continues with the groups that haven't yielded yet.

**Raises**:

- `BatchError` - If ``events()`` or ``wait_for_quorum()`` was called first, or this handle has
  been detached or released, when called or while waiting.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.wait_for_quorum"></a>

##### wait\_for\_quorum

```python
def wait_for_quorum() -> list[Member]
```

Waits until every group has ``min_ready`` Ready members, and returns them.

The returned list has each group's first ``min_ready`` members, in the order they became
Ready, with the groups in batch order. Members it doesn't return are yielded by ``events()``.
If the quorum fails, no members are handed out and no more claims are created. The quorum
also fails if the wait ends with any other exception, such as a cancellation or
``KeyboardInterrupt``. Can be called only once per handle.

``QuorumUnreachableError`` and ``BatchTimeoutError`` are both ``BatchError``, so
``except BatchError`` handles every failure.

**Raises**:

- `QuorumUnreachableError` - If too few members of a group can still become Ready.
- `BatchTimeoutError` - If the quorum isn't reached within the quorum timeout.
- `BatchError` - If ``events()`` or ``iter_ready_groups()`` was called first, this method
  was already called, ``err()`` is set while waiting, or this handle has been
  detached or released, when called or while waiting.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.acquire"></a>

##### acquire

```python
def acquire(warmpool: str, timeout: float | None = None) -> Member
```

Adds a member from ``warmpool`` to the batch, waits until it is Ready, and returns it.

The pool can be any warm pool in the batch's namespace. A pool that isn't one of the batch's
groups is checked once per handle, like ``claim_batch`` checks its pools. The claim takes the
next unused ordinal and carries its pool's group label but no group annotations, so it is
never one of the initial claims. ``events()``, ``iter_ready_groups()``, ``wait_for_quorum()``
and ``release_not_ready()`` leave it alone, and ``release_group()`` on its pool releases it
with the pool's other members. Its ``shutdownTime`` is the
time of its create plus ``quorum_timeout`` plus ``work_budget`` plus 600 seconds, and its
create is retried and paced like the initial claims'.

If it raises, including on a cancellation, the member is dropped from ``members()`` and its
claim is deleted, unless the claim is already gone or its create was rejected. A failed delete is
retried by ``release_not_ready()`` and ``release()``. It never creates a second claim, so
call it again to try again. A member it returned that fails later shows in ``members()``
like any other, and ``release_member`` frees it.

``TerminalMemberError`` and ``BatchTimeoutError`` are both ``BatchError``, so
``except BatchError`` handles every runtime failure. The other errors mean a bad argument
or a missing pool or template.

**Arguments**:

- `warmpool` - The warm pool to claim the member from.
- `timeout` - Seconds to wait for the member to be Ready. Defaults to the batch's
  ``quorum_timeout``.
  

**Raises**:

- `ValueError` - If ``timeout`` isn't a positive finite number.
- `SandboxWarmPoolNotFoundError` - If the warm pool doesn't exist.
- `SandboxTemplateNotFoundError` - If the warm pool's template doesn't exist, or it names none.
- `TerminalMemberError` - If the create fails (the member is ``FAILED`` with reason
  ``CreateFailed``), the member fails terminally, or someone else deletes its claim
  (the member is ``LOST``) before it is Ready.
- `BatchTimeoutError` - If the member isn't Ready within ``timeout``. A create or create retry
  that ``create_rps`` pacing can't start within it isn't sent.
- `BatchError` - If this handle has been detached or released, or ``err()`` is set, when
  called or while waiting, or if ``release_member`` or ``release_group`` releases the
  member before it is Ready.

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.release_member"></a>

##### release\_member

```python
def release_member(member: Member) -> None
```

Deletes one member's claim and drops the member from ``members()``.

The member is never handed out afterwards and emits no ``MEMBER_LOST``. A member of the
initial claims counts as unable to become Ready, so releasing one before a quorum consumer
has decided can make its group or the quorum unreachable. A claim that is already gone
counts as deleted, and transient errors are retried. If the delete fails, calling
``release_member`` again retries it.

**Raises**:

- `ValueError` - If the member isn't in this batch.
- `BatchError` - If this handle has been detached or released.
- `ApiException` - For a non-transient error such as a 403, or the last transient error
  after ``BATCH_DELETE_ATTEMPTS`` attempts (an ``ApiException`` or a transport error).

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.release_not_ready"></a>

##### release\_not\_ready

```python
def release_not_ready() -> None
```

Ends the fill and deletes every initial claim that isn't Ready and was never handed out.

Creates not yet sent are skipped, and a create that lands afterwards is deleted. A member
that was handed out is kept, even if it has since gone not Ready. Members are picked and
released in one step under the batch lock, so none can turn Ready and be handed out in
between, as could happen when calling ``release_member`` on each member that ``members()``
lists as not Ready. Released members are dropped from ``members()``, emit no
``MEMBER_LOST``, and count as unable to become Ready, so calling this before a quorum
consumer has decided can make a group or the quorum unreachable. Call it once you have
what you need.

Each claim is deleted like ``release_member``, up to ``BATCH_DEFAULT_MAX_IN_FLIGHT`` at
once. If a delete fails, calling ``release_not_ready`` again retries it.

**Raises**:

- `BatchError` - If this handle has been detached or released.
- `ApiException` - The first error a delete ended with, once every delete has been tried
  (an ``ApiException`` or a transport error).

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.release_group"></a>

##### release\_group

```python
def release_group(warmpool: str) -> None
```

Deletes every claim the batch holds from one pool, pending ones included, and drops their
members from ``members()``. That covers the group's initial claims and the members
``acquire()`` added from the pool, and an ``acquire()`` still waiting on the pool raises
``BatchError``.

The group's creates not yet sent are skipped, and a create that lands afterwards is deleted.
Its members are never handed out afterwards and emit no ``MEMBER_LOST``. The initial ones
count as unable to become Ready, so releasing a group before a quorum consumer has decided
makes it, or the quorum, unreachable. Works with any consumer, typically on a group that
``iter_ready_groups()`` yields with an error.

Claims are deleted like ``release()`` deletes the batch's, with deletecollection rounds
scoped to the group's label. If they fail, calling ``release_group`` again retries.

**Raises**:

- `ValueError` - If the batch has no group, member or ``acquire()`` in flight for ``warmpool``.
- `BatchError` - If this handle has been detached or released, or claims are still not
  being deleted after the last round.
- `ApiException` - For a non-transient error such as a 403, which is raised at once.
  After the last round, the last transient error is raised (an ``ApiException``
  or a transport error).

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.release"></a>

##### release

```python
def release() -> None
```

Stops creating claims, stops the background watch and Lease renewal, closes cached
sandbox connections, then deletes the batch's claims and finally its Lease. Idempotent.

Claims are deleted with label-scoped deletecollection requests, re-listing between rounds
until no claim is left that isn't already being deleted. A transient error moves on to the
next round, and release gives up after ``BATCH_RELEASE_MAX_IDLE_ROUNDS`` rounds in a row
that delete nothing. On failure the Lease is kept, and calling ``release`` again retries.

**Raises**:

- `BatchError` - If this handle has been detached, or claims are still not being deleted
  after the last round.
- `ApiException` - For a non-transient error such as a 403, which is raised at once.
  After the last round, the last transient error is raised (an ``ApiException``
  or a transport error).

<a id="k8s_agent_sandbox.sandbox_batch.SandboxBatch.detach"></a>

##### detach

```python
def detach(grace: int | None = None) -> None
```

Stops the background watch and Lease renewal, closes cached sandbox connections, and
releases this handle's hold on the Lease. ``grace`` is the number of seconds to keep the
Lease valid after detaching, an int greater than ``CLOCK_SKEW_MARGIN`` (5); if ``None``, we
use the batch's original Lease duration.

If the Lease release fails, the error propagates and this handle stays in a detached state with
the Lease still held. Call ``detach`` again to retry; steps that already completed are skipped.
A batch that is still creating claims stops creating first, and every claim is left in place.

**Raises**:

- `ValueError` - If ``grace`` is not a valid Lease duration.
- `BatchError` - If this handle has been released.
- `ApiException` - If the Lease release fails (an ``ApiException`` or a transport error).

