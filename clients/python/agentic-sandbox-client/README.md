# Agentic Sandbox Client Python

This Python client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It's designed to be used as a context manager,
ensuring that sandbox resources are properly created and cleaned up.

It supports a **scalable, cloud-native architecture** using Kubernetes Gateways and a specialized
Router, while maintaining a convenient **Tunnel Mode** for local testing.

## Architecture

The client operates in four connectivity modes:

1.  **Gateway Mode:** Traffic flows from the Client -> Cloud Load Balancer (Gateway)
    -> Router Service -> Sandbox Pod. This supports external ingress via Gateway API.
2.  **Tunnel Mode:** Traffic flows from Localhost -> `kubectl port-forward` -> Router
    Service -> Sandbox Pod. This requires no public IP and works on Kind/Minikube for local development.
3.  **In-Cluster Mode:** The client connects **directly to the sandbox pod** (via pod IP or cluster
    DNS), bypassing the router. Intended for workloads running inside the cluster.
4.  **Direct URL Mode:** The client connects directly to a provided `api_url`, bypassing
    discovery. This is useful when connecting through a custom domain or a manually specified router URL.

## Prerequisites

- A running Kubernetes cluster.
- The [**Agent Sandbox Controller**](https://github.com/kubernetes-sigs/agent-sandbox?tab=readme-ov-file#installation) installed.
- `kubectl` installed and configured locally.

## Setup: Deploying the Router

Before using the client in Gateway Mode or Tunnel Mode, deploy the `sandbox-router` into your cluster.

1.  **Deploy the Router:**

    Follow the instructions in [sandbox-router](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router) to deploy the router using the manifests in [sandbox-router/deploy](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router/deploy). *(Note: If you installed a specific client release tag, replace `main` in these URLs with the corresponding tag.)*

2.  **Create a Sandbox Warmpool:**

    Ensure a `SandboxWarmPool` exists in your target namespace. The test_client.py
    uses the [python-runtime-sandbox](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/python-runtime-sandbox) image.

    ```bash
    kubectl apply -f python-sandbox-warmpool.yaml
    ```

## Installation

1.  **Create a virtual environment:**

    ```bash
    python3 -m venv .venv
    source .venv/bin/activate
    ```

2.  **Install Agent Sandbox Client**
    

    * **Option 1: Install from PyPI (Recommended):**

        The package is available on [PyPI](https://pypi.org/project/k8s-agent-sandbox/) as `k8s-agent-sandbox`.

        ```bash
        pip install k8s-agent-sandbox
        ```

        If you are using [tracing with GCP](GCP.md#tracing-with-open-telemetry-and-google-cloud-trace), install with the optional tracing dependencies:

        ```bash
        pip install "k8s-agent-sandbox[tracing]"
        ```


    * **Option 2: Install from source via git:**

        ```bash
        # Replace "main" with a specific version tag (e.g., "v0.1.0") from
        # https://github.com/kubernetes-sigs/agent-sandbox/releases to pin a version tag.
        export VERSION="main"

        pip install "git+https://github.com/kubernetes-sigs/agent-sandbox.git@${VERSION}#subdirectory=clients/python/agentic-sandbox-client"
        ```

        **Note**: This package uses `setuptools-scm` for dynamic versioning. For Option 2 and Option 3, when installing locally, you may notice the version increment if your local repository has uncommitted changes or is ahead of the last tagged release. This is expected behavior to ensure unique versioning during development.

    * **Option 3: Install from source in editable mode:**

        If you have not already done so, first clone this repository:

        ```bash
        cd ~
        git clone https://github.com/kubernetes-sigs/agent-sandbox.git
        cd agent-sandbox/clients/python/agentic-sandbox-client
        ```

        And then install the agentic-sandbox-client into your activated .venv:

        ```bash
        pip install -e .
        ```

        If you are using [tracing with GCP](GCP.md#tracing-with-open-telemetry-and-google-cloud-trace),
        install with the optional tracing dependencies:

        ```bash
        pip install -e ".[tracing]"
        ```

## Usage Examples

### 1. Gateway Mode (GKE Gateway)

Use this when running against a real cluster with a public Gateway IP. The client automatically
discovers the Gateway.

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig

# Connect via the GKE Gateway
client = SandboxClient(
    connection_config=SandboxGatewayConnectionConfig(
        gateway_name="external-http-gateway",  # Name of the Gateway resource
    )
)

sandbox = client.create_sandbox(warmpool="python-sandbox-warmpool", namespace="default")
try:
    print(sandbox.commands.run("echo 'Hello from Cloud!'").stdout)
finally:
    sandbox.terminate()
```

### 2. Tunnel Mode (Local Port-Forward)

Use this for local development or CI. The client automatically opens a secure tunnel to the
Router Service using `kubectl`.

> **Namespace note:** `router_namespace` controls *where the router service lives*
> (default: `"agent-sandbox-system"`). This is separate from the `namespace` argument
> passed to `create_sandbox`, which controls *where sandbox pods are scheduled*.
> If you deployed the router into a different namespace (e.g. `"default"`), you must
> set `router_namespace` accordingly — otherwise `kubectl port-forward` will fail with
> `"services sandbox-router-svc not found"`.

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxLocalTunnelConnectionConfig

# Router deployed in the default agent-sandbox-system namespace:
client = SandboxClient(
    connection_config=SandboxLocalTunnelConnectionConfig()
)

# If the router is deployed in a different namespace (e.g. "default"):
# client = SandboxClient(
#     connection_config=SandboxLocalTunnelConnectionConfig(router_namespace="default")
# )

sandbox = client.create_sandbox(warmpool="python-sandbox-warmpool", namespace="default")
try:
    print(sandbox.commands.run("echo 'Hello from Local!'").stdout)
finally:
    sandbox.terminate()
```

You can pass per-claim environment variables when creating a sandbox:

```python
sandbox = client.create_sandbox(
    warmpool="python-sandbox-warmpool",
    namespace="default",
    env={"FOO": "bar"},
)
```

Setting `env` populates `SandboxClaim.spec.env`, which forces a cold start
from the warm pool template instead of adopting a pre-warmed pod. This may
increase startup latency.

### 3. Legacy Runtime In-Cluster Mode (Direct Pod Connection)

Use this when the client runs **inside the cluster** (for example, another pod in the same cluster).
The client connects **directly to the sandbox runtime pod**, bypassing the sandbox router.

The client first uses the pod IP reported in the Sandbox status. If the pod IP is not available
(for example, before status is populated or when running against an older controller), it falls
back to the stable cluster DNS endpoint:
`http://{sandbox_id}.{namespace}.svc.cluster.local:{server_port}`.

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxInClusterConnectionConfig

connection_config = SandboxInClusterConnectionConfig()

client = SandboxClient(connection_config=connection_config)

sandbox = client.create_sandbox(warmpool="python-sandbox-warmpool", namespace="default")
try:
    print(sandbox.commands.run("echo 'Hello from in-cluster!'").stdout)
finally:
    sandbox.terminate()
```

### 4. Direct URL Mode

Use `SandboxDirectConnectionConfig` to bypass discovery entirely. Useful for:

- **Internal Agents:** Running inside the cluster (e.g. router Service DNS).
- **Custom Domains:** Connecting via HTTPS (e.g., `https://sandbox.example.com`).

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig

client = SandboxClient(
    connection_config=SandboxDirectConnectionConfig(
       api_url="http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080"
    )
)

sandbox = client.create_sandbox(warmpool="python-sandbox-warmpool", namespace="default")
try:
    sandbox.commands.run("ls -la")
finally:
    sandbox.terminate()
```

If the router sits behind an authenticating gateway, add headers to every request
with `extra_headers`, and enable mTLS with `client_cert` and `ca_cert`. The TLS
options require an `https://` URL, and `X-Sandbox-*` header names are reserved.

```python
SandboxDirectConnectionConfig(
    api_url="https://sandbox.example.com",
    extra_headers={"Authorization": "Bearer <token>"},
    client_cert=("/path/to/client.crt", "/path/to/client.key"),
    ca_cert="/path/to/ca.crt",  # omit to use the default trust store
)
```

### 5. Custom Ports

If your sandbox runtime listens on a port other than 8888 (e.g., a Node.js app on 3000), specify `server_port`.

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxLocalTunnelConnectionConfig

client = SandboxClient(
    connection_config=SandboxLocalTunnelConnectionConfig(server_port=3000)
)

sandbox = client.create_sandbox(warmpool="node-sandbox-warmpool", namespace="default")
```

### File Operations

`read()` remains convenient for small files and returns the complete contents as
`bytes`. Use `read_to()` for large files so the response is copied incrementally
into a caller-owned binary destination:

```python
with open("artifact-copy.tar", "wb") as destination:
    written = sandbox.files.read_to(
        "artifact.tar",
        destination,
        max_bytes=512 * 1024 * 1024,
    )

print(f"downloaded {written} bytes")
```

`read_to()` never closes the destination. It always closes the HTTP response,
including after a size-limit violation or destination error. If an error occurs,
data already written remains in the destination. Omitting `max_bytes` disables
the optional per-call download limit.

`AsyncFilesystem.read_to()` provides the same behavior for an asynchronous sink
whose `write(bytes)` method is awaitable and returns the number of bytes accepted:

```python
written = await sandbox.files.read_to(
    "artifact.tar",
    async_destination,
    max_bytes=512 * 1024 * 1024,
)
```

### 6. Async Client

For async applications (FastAPI, aiohttp, async agent orchestrators), use the `AsyncSandboxClient`.
Install the async extras first:

```bash
pip install k8s-agent-sandbox[async]
```

The async client requires an explicit connection config — `SandboxLocalTunnelConnectionConfig`
is not supported because it relies on a synchronous `kubectl port-forward` subprocess. Use
`SandboxGatewayConnectionConfig`, `SandboxDirectConnectionConfig`,
`SandboxInClusterConnectionConfig`, `SandboxdPodTunnelConnectionConfig`, or
`SandboxdInClusterConnectionConfig`. For the portable
`sandboxd` runtime, install
both optional extras: `pip install 'k8s-agent-sandbox[async,grpc]'`.

**Direct connection (explicit URL, e.g. router service):**

```python
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig

async def main():
    config = SandboxDirectConnectionConfig(
        api_url="http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080"
    )

    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            warmpool="python-sandbox-warmpool",
            namespace="default",
        )
        result = await sandbox.commands.run("echo 'Hello from async!'")
        print(result.stdout)

asyncio.run(main())
```

**Legacy runtime in-cluster (Pod IP first, then constructed cluster DNS):**

```python
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxInClusterConnectionConfig

async def main():
    config = SandboxInClusterConnectionConfig()

    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            warmpool="python-sandbox-warmpool",
            namespace="default",
        )
        result = await sandbox.commands.run("echo 'Hello from async!'")
        print(result.stdout)

asyncio.run(main())
```

**sandboxd runtime (direct pod tunnel):**

`SandboxdPodTunnelConnectionConfig` forwards sandboxd's REST filesystem port and gRPC
process port directly from the sandbox Pod. The async client establishes and tears down
both forwards without blocking the event loop.

This doesn't work on Kata or gVisor: their workload runs in a separate kernel, so the
pod's host-side network namespace has nothing listening for the port-forward to reach.
Use `SandboxdInClusterConnectionConfig` for those, with the client running inside the
cluster (the Sandbox NetworkPolicy must admit the client; see below).

```python
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig

async def main():
    config = SandboxdPodTunnelConnectionConfig()
    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            warmpool="sandboxd-warmpool",
            namespace="default",
        )
        result = await sandbox.commands.run("echo 'Hello from sandboxd'")
        await sandbox.files.write("hello.txt", result.stdout)

asyncio.run(main())
```

### 7. Streaming File Uploads

`write` accepts strings, bytes, and binary file objects. Passing a file object
streams it from its current position instead of loading the entire file into
client memory:

```python
with open("dataset.tar", "rb") as source:
    sandbox.files.write("data/dataset.tar", source)
```

The async client accepts the same binary file objects. File reads run outside
the event loop while each chunk is uploaded:

```python
with open("dataset.tar", "rb") as source:
    await sandbox.files.write("data/dataset.tar", source)
```

The caller owns the file object and must close it. A streaming upload is sent
once because an arbitrary stream cannot be replayed safely after a partial
request. If the upload fails, its file position may have advanced. File-object
uploads use HTTP chunked transfer encoding, so the runtime and any intermediary
must accept request bodies without a `Content-Length` header.

### 8. sandboxd In-Cluster Mode

Use `SandboxdInClusterConnectionConfig` when the Python client runs in the same
Kubernetes cluster as a sandboxd-backed Sandbox. Select `service-dns` or `pod-ip`
explicitly. The SDK reads the selected address from Sandbox status, uses it for
both the REST filesystem API and gRPC `ProcessService`, and never switches to
the other mode. The default ports are 8080 and 9090; `rest_port` and
`grpc_port` can be overridden independently. This mode uses no `kubectl`
process or sandbox-router headers.
Direct access bypasses the sandbox-router's authorization checks, so restrict
network access to trusted client workloads.

For Service DNS, set `spec.service: true` on the Sandbox template so the
controller reports `status.serviceFQDN`. If it is absent, the SDK raises
`SandboxServiceUnavailableError`. For Pod IP, the SDK reads `status.podIPs`
before each operation; an unavailable address raises `SandboxNotReadyError`.
Pod IPs can be recycled after a Pod is replaced, so prefer Service DNS when
clients and Sandboxes cross trust boundaries.

The client workload needs permission to `get` `sandboxes.agents.x-k8s.io` in
the Sandbox namespace. The Sandbox NetworkPolicy must allow the client to reach
the sandboxd REST and gRPC ports (TCP 8080 and 9090 by default). The default
managed policy in the [sandboxd example](../../../examples/sandboxd-sandbox/sandbox-template.yaml)
only admits the sandbox-router; direct clients need an explicit ingress rule.
If client egress is restricted, allow its traffic to these ports and, for
Service DNS mode, to cluster DNS.
When supplying `spec.networkPolicy`, preserve any other required rules because
it replaces the default policy.

Synchronous client (install `k8s-agent-sandbox[grpc]`):

```python
from k8s_agent_sandbox import SandboxClient, SandboxdInClusterConnectionConfig

client = SandboxClient(
    connection_config=SandboxdInClusterConnectionConfig(mode="service-dns")
)
sandbox = client.create_sandbox(warmpool="sandboxd-warmpool", namespace="default")
try:
    sandbox.files.write("hello.txt", b"hello\n")
    print(sandbox.commands.run("cat hello.txt").stdout)
finally:
    sandbox.terminate()
```

Asynchronous client (install `k8s-agent-sandbox[async,grpc]`):

```python
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient, SandboxdInClusterConnectionConfig

async def main():
    config = SandboxdInClusterConnectionConfig(mode="pod-ip")
    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            warmpool="sandboxd-warmpool", namespace="default"
        )
        await sandbox.files.write("hello.txt", b"hello\n")
        print((await sandbox.commands.run("cat hello.txt")).stdout)

asyncio.run(main())
```

### 9. Labels and Pod Metadata

`create_sandbox` lets you attach metadata at two different levels:

- `labels`: Kubernetes labels on the **SandboxClaim object** itself
  (`SandboxClaim.metadata.labels`). Useful for selecting/listing claims.
- `pod_labels` / `pod_annotations`: labels and annotations stamped onto the
  running Sandbox **Pod** via `spec.additionalPodMetadata`. Because they live on
  the Pod, the workload can read them from inside the sandbox through the
  [Downward API](https://kubernetes.io/docs/concepts/workloads/pods/downward-api/)
  (for example, to stamp a tenant or client identifier and reject requests that
  don't belong to it).

```python
sandbox = client.create_sandbox(
    warmpool="python-sandbox-warmpool",
    namespace="default",
    labels={"team": "platform"},            # on the SandboxClaim object
    pod_labels={"client-id": "tenant-a"},   # on the running Pod
    pod_annotations={"owner": "tenant-a"},  # on the running Pod
)
```

`pod_labels` are validated with the same Kubernetes label rules as `labels`. The
same parameters are available on `AsyncSandboxClient.create_sandbox`.

Behavioral notes:

- A `pod_label` / `pod_annotation` whose key already exists on the warmpool
  template with a different value is rejected by the controller's "No
  Overrides" rule, and the reconcile errors.
- Client-side validation only checks RFC-1123 label syntax. The controller's
  domain allow-list and system-label restrictions are enforced server-side and
  are not replicated client-side.

### 10. Deterministic, Retry-Safe Claim Creation

By default, `create_sandbox` continues to generate a new random
`SandboxClaim` name for every call. For a workflow that may retry after an
ambiguous response or process failover, pass a stable `claim_name` and opt in
to safe adoption:

```python
sandbox = client.create_sandbox(
    warmpool="python-sandbox-warmpool",
    namespace="default",
    claim_name="sandbox-workflow-123",
    adopt_existing=True,
    labels={"workflow": "workflow-123"},
)
```

The same keyword arguments are available on
`AsyncSandboxClient.create_sandbox`. A deterministic name must be a valid
Kubernetes DNS-1123 subdomain of at most 253 characters.
`adopt_existing=True` requires an explicit `claim_name`.

Adoption happens only after HTTP `409 Conflict`. The client reads the existing
Claim, checks that it references the requested warm pool and is not terminating,
then evaluates readiness. An already-ready Claim needs no further watch event;
otherwise the watch starts from the observed resource version. The UID observed
during explicit creation or adoption is checked on watch events, including
after an expired watch restarts. If the Claim disappears between the conflict
and the read, `SandboxNotFoundError` tells the caller to retry.

Creation options such as labels, Pod metadata, environment variables and volume
templates are not reapplied or compared on adoption: the Claim spec is mutable.
`shutdown_after_seconds` can be used with adoption; an existing Claim keeps its
original `shutdownTime`, so retries do not extend its lifetime. Use a distinct
Claim name when you need a new allocation with different settings.

Claims explicitly named through this client's `create_sandbox()` are
caller-owned, including after a failed attempt. They are not deleted on a
readiness failure, context-manager exit or `atexit`. Use `delete_sandbox()` or
`delete_all()` for deliberate deletion. Reattachment through `get_sandbox()` on
a new client retains its existing cleanup behavior.

Random-name creation retains its existing cleanup behavior, including rollback
when the create response is lost. Rollback uses a UID precondition when the
response provided a UID; without one it retains the existing name-based delete.
A `409` does not trigger rollback of the conflicting Claim. This feature does
not add thread-safety guarantees or UID tracking to returned handles.

### 11. Custom Volume Claim Templates

You can dynamically request persistent volumes to be attached to your Sandbox Pod by specifying `volume_claim_templates`. This allows the sandbox to mount custom PersistentVolumeClaims (PVCs).

```python
sandbox = client.create_sandbox(
    warmpool="python-sandbox-warmpool",
    namespace="default",
    volume_claim_templates=[
        {
            "metadata": {
                "name": "my-volume",
            },
            "spec": {
                "accessModes": ["ReadWriteOnce"],
                "resources": {
                    "requests": {
                        "storage": "1Gi",
                    },
                },
            },
        }
    ],
)
```

The volume claim templates are validated against the warmpool template's policy and rules (e.g., whether custom volume claims are allowed or if overrides are permitted).

### 12. Startup Latency: How the SDK Waits for Readiness

`create_sandbox()` is fully **watch-based** — it never polls the Kubernetes
API on an interval, so there is no poll-interval latency added on top of the
controller's own claim-to-Ready time.

The wait is a **single watch on the SandboxClaim**. The claim controller
publishes the bound sandbox name (`status.sandbox.name`), the pod IPs
(`status.sandbox.podIPs`) and the forwarded `Ready` condition in one status
update when it adopts a warm-pool sandbox, so the first watch event that
carries the sandbox name normally also carries `Ready=True` and
`create_sandbox()` returns immediately. On a cold start (no warm sandbox
available, or `env`/`volume_claim_templates` set, which force cold starts)
the same watch simply keeps streaming claim updates until the forwarded
`Ready` condition flips to `True`.

Latency guidance:

- **Do not poll** `Sandbox`/`SandboxClaim` objects with `get_*` calls in a
  loop to detect readiness; a poll interval of `T` adds an average of `T/2`
  (uniformly distributed 0..`T`) on top of the controller latency. Use
  `create_sandbox()` / the claim `Ready` condition watch.
- `sandbox_ready_timeout` (default 180s) bounds the whole wait; the watch
  returns as soon as the claim is Ready, the timeout only caps the worst case.
- The Kubernetes client reuses a single authenticated connection pool for
  the watch, so no extra TLS handshakes occur on the ready path.
- With the local-tunnel connection mode, the first request additionally pays
  for the `kubectl port-forward` startup; the SDK probes the local port every
  50ms while it comes up. Gateway/in-cluster modes do not have this step.

### 13. Targeting a Specific Cluster or Context

By default, `SandboxClient` and `AsyncSandboxClient` load their Kubernetes credentials via an in-cluster config if running inside a pod, otherwise `KUBECONFIG`, falling back to `~/.kube/config`'s `current-context` if unset. To target a different cluster/context instead, pass a pre-configured `api_client`.

> **Warning:** In local-tunnel mode, `api_client` only affects Kubernetes API calls. The `kubectl` calls that reach the sandbox-router (the service check and the port-forward) still use your ambient kubeconfig context, so make sure it points at the same cluster as `api_client`, or they will target the wrong cluster.

**A kubeconfig file outside the default location** (e.g. a `pytest-kind` cluster):

```python
from kubernetes import client, config
from k8s_agent_sandbox import SandboxClient

cfg = client.Configuration()
config.load_kube_config(
    config_file="/path/to/other-kubeconfig.yaml",  # not KUBECONFIG/~/.kube/config
    context="my-cluster-context",
    client_configuration=cfg,
)

sandbox_client = SandboxClient(api_client=client.ApiClient(configuration=cfg))
```

**Multiple clusters in one process** — each `SandboxClient` needs its own `api_client`, built from its own `Configuration`; constructing one doesn't affect another already in use:

```python
from kubernetes import client, config
from k8s_agent_sandbox import SandboxClient

def build_client(context: str) -> SandboxClient:
    cfg = client.Configuration()
    config.load_kube_config(context=context, client_configuration=cfg)
    return SandboxClient(api_client=client.ApiClient(configuration=cfg))

client_a = build_client("cluster-a")
client_b = build_client("cluster-b")
```

The async client takes the same parameter with a `kubernetes_asyncio.client.ApiClient`. If its credentials can expire, use `async with AsyncSandboxClient(...)` or call `delete_all()` before the event loop stops, since `atexit` cleanup can't run an async token-refresh hook and may delete with a stale token.

An injected `api_client` is caller-owned: the SDK never closes it, so closing it is up to you.

## Testing

A test script is included to verify the full lifecycle (Creation -> Execution -> File I/O -> Cleanup).

### Run in Tunnel Mode:

```bash
python test_client.py --namespace default
```

### Run in Gateway Mode:

```bash
python test_client.py --gateway-name external-http-gateway
```
