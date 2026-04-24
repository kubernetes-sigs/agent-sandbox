---
title: "SDK Reference"
linkTitle: "SDK Reference"
weight: 15
description: >
  Agent Sandbox Python SDK Reference.
---
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
  all tracked sandboxes when the program terminates. Defaults to False.

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.create_sandbox"></a>

##### create\_sandbox

```python
def create_sandbox(template: str,
                   namespace: str = "default",
                   sandbox_ready_timeout: int = 180,
                   labels: dict[str, str] | None = None,
                   *,
                   shutdown_after_seconds: int | None = None) -> T
```

Provisions new Sandbox claim and returns a Sandbox handle which tracks
the underlying infrastructure.

**Arguments**:

- `template` - Name of the SandboxTemplate to use.
- `namespace` - Kubernetes namespace for the claim.
- `sandbox_ready_timeout` - Seconds to wait for the sandbox to be ready.
- `labels` - Optional Kubernetes labels to attach to the claim.
- `shutdown_after_seconds` - Optional TTL in seconds. When set, the
  claim's ``spec.lifecycle`` is populated with a ``shutdownTime``
  of *now + shutdown_after_seconds* (UTC) and a ``shutdownPolicy``
  of ``"Delete"``, so the controller auto-deletes the claim on
  expiry. Must be a positive integer.
  

**Example**:

  
  >>> client = SandboxClient()
  >>> sandbox = client.create_sandbox(template="python-sandbox-template")
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

**Example**:

  
  >>> client = SandboxClient()
  >>> sandbox = client.get_sandbox("sandbox-claim-1234abcd")
  >>> sandbox.commands.run("ls -la")

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.list_active_sandboxes"></a>

##### list\_active\_sandboxes

```python
def list_active_sandboxes() -> List[Tuple[str, str]]
```

Returns a list of tuples containing (namespace, claim_name) currently managed by this client.

**Example**:

  
  >>> client = SandboxClient()
  >>> client.create_sandbox("python-sandbox-template")
  >>> print(client.list_active_sandboxes())
  [('default', 'sandbox-claim-1234abcd')]

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.list_all_sandboxes"></a>

##### list\_all\_sandboxes

```python
def list_all_sandboxes(namespace: str = "default") -> List[str]
```

Lists all SandboxClaim names currently existing in the Kubernetes cluster
for the given namespace.

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
  >>> sandbox = client.create_sandbox("python-sandbox-template")
  >>> client.delete_sandbox(sandbox.claim_name)

<a id="k8s_agent_sandbox.sandbox_client.SandboxClient.delete_all"></a>

##### delete\_all

```python
def delete_all()
```

Cleanup all tracked sandboxes managed by this client.

**Example**:

  
  >>> client = SandboxClient()
  >>> client.create_sandbox("python-sandbox-template")
  >>> client.create_sandbox("python-sandbox-template")
  >>> client.delete_all()

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

<a id="k8s_agent_sandbox.models.FileEntry.name"></a>

##### name

Name of the file.

<a id="k8s_agent_sandbox.models.FileEntry.size"></a>

##### size

Size of the file in bytes.

<a id="k8s_agent_sandbox.models.FileEntry.type"></a>

##### type

Type of the entry (file or directory).

<a id="k8s_agent_sandbox.models.FileEntry.mod_time"></a>

##### mod\_time

Last modification time of the file. (POSIX timestamp)

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
