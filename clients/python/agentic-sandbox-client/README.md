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

`SandboxClient` is also a context manager. On exit it deletes the sandboxes it
tracks, including ones attached with `get_sandbox()`, so the `try`/`finally`
above is not needed. Claims created with an explicit `claim_name` are
caller-owned and are only disconnected, not deleted:

```python
with SandboxClient(connection_config=SandboxLocalTunnelConnectionConfig()) as client:
    sandbox = client.create_sandbox(warmpool="python-sandbox-warmpool", namespace="default")
    print(sandbox.commands.run("echo 'Hello from Local!'").stdout)
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

Use `SandboxFileConfig` to bound file downloads for every sandbox created or
attached by a client:

```python
from k8s_agent_sandbox.models import SandboxFileConfig

client = SandboxClient(
    file_config=SandboxFileConfig(
        max_download_size=256 * 1024 * 1024,
    ),
)
sandbox = client.create_sandbox(warmpool="python-sandbox-warmpool")
```

`AsyncSandboxClient` accepts the same `file_config`; it can also be passed to
`Sandbox` or `AsyncSandbox` directly. `max_download_size` is a positive byte
limit shared by `read()` and `read_to()`, for both runtimes. It defaults to
`None`, preserving unlimited downloads. An oversized download raises
`SandboxResponseTooLargeError` (a `RuntimeError` subclass) and closes the response.
Limits are checked while reading, including when the response length is unknown.
The limit counts file bytes after HTTP content decoding, not compressed wire bytes.

`read()` remains convenient for small files and returns the complete contents as
`bytes`; it never returns partial content on overflow. Use `read_to()` for large
files so the response is copied incrementally into a caller-owned binary destination:

```python
with open("artifact-copy.tar", "wb") as destination:
    written = sandbox.files.read_to(
        "artifact.tar",
        destination,
        max_bytes=128 * 1024 * 1024,
    )

print(f"downloaded {written} bytes")
```

`read_to()` never closes the destination. It always closes the HTTP response,
including after a size-limit violation or destination error. If an error occurs,
data already written remains in the destination. The smaller of the configured
`max_download_size` and the per-call `max_bytes` applies. Omitting `max_bytes`
removes only the per-call restriction, not the configured limit; `max_bytes=0`
allows only empty files. With neither limit set, downloads remain unlimited.

`AsyncFilesystem.read_to()` provides the same behavior for an asynchronous sink
whose `write(bytes)` method is awaitable and returns the number of bytes accepted:

```python
written = await sandbox.files.read_to(
    "artifact.tar",
    async_destination,
    max_bytes=128 * 1024 * 1024,
)
```

On the sandboxd runtime, `files.mkdir(path, parents=False)`,
`files.move(source, destination, overwrite=False)` and
`files.copy(source, destination, overwrite=False)` create, rename and copy
paths. The legacy runtime raises `NotImplementedError`.

### 6. Async Client

For async applications (FastAPI, aiohttp, async agent orchestrators), use the `AsyncSandboxClient`.
Install the async extras first:

```bash
pip install k8s-agent-sandbox[async]
```

The async client requires an explicit connection config — `SandboxLocalTunnelConnectionConfig`
is not supported because it relies on a synchronous `kubectl port-forward` subprocess. Use
`SandboxGatewayConnectionConfig`, `SandboxDirectConnectionConfig`,
`SandboxInClusterConnectionConfig`, `SandboxdPodTunnelConnectionConfig`,
`SandboxdRouterTunnelConnectionConfig`, or
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
Use `SandboxdRouterTunnelConnectionConfig` for those from outside the cluster.
Use `SandboxdInClusterConnectionConfig` when the client runs inside the cluster
(the Sandbox NetworkPolicy must admit the client; see below).

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
Kubernetes cluster as a sandboxd-backed Sandbox. Select `in-cluster-service` or
`in-cluster-pod-ip` explicitly. The SDK reads the selected address from Sandbox
status, uses it for both the REST filesystem API and gRPC `ProcessService`, and
never switches to the other mode. The default ports are 8080 and 9090;
`rest_port` and `grpc_port` can be overridden independently. This mode uses no
`kubectl` process or sandbox-router headers.
Direct access bypasses the sandbox-router's authorization checks, so restrict
network access to trusted client workloads.

For Service DNS, set `spec.service: true` on the Sandbox template so the
controller reports `status.serviceFQDN`. If it is absent, the SDK raises
`SandboxNoServiceError`. For Pod IP, the SDK reads `status.podIPs`
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
    connection_config=SandboxdInClusterConnectionConfig(mode="in-cluster-service")
)
sandbox = client.create_sandbox(warmpool="sandboxd-warmpool", namespace="default")
try:
    sandbox.files.write("hello.txt", b"hello\n")
    print(sandbox.commands.run("cat hello.txt").stdout)
    sandbox.commands.run("mkdir -p work")
    sandbox.commands.run("echo $GREETING > hi.txt", env={"GREETING": "hi"}, cwd="work")
finally:
    sandbox.terminate()
```

Asynchronous client (install `k8s-agent-sandbox[async,grpc]`):

```python
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient, SandboxdInClusterConnectionConfig

async def main():
    config = SandboxdInClusterConnectionConfig(mode="in-cluster-pod-ip")
    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            warmpool="sandboxd-warmpool", namespace="default"
        )
        await sandbox.files.write("hello.txt", b"hello\n")
        print((await sandbox.commands.run("cat hello.txt")).stdout)

asyncio.run(main())
```

With sandboxd, `commands.run` also takes `env` and `cwd`, as in the synchronous
example. `cwd` is confined to the sandbox root. The legacy runtime raises
`NotImplementedError` for both.

#### sandboxd bearer authentication

If sandboxd requires a bearer token (`--auth-token-sha256` or
`--auth-token-file`), set `auth_token` on either
`SandboxdPodTunnelConnectionConfig` or `SandboxdInClusterConnectionConfig`:

```python
import os
from k8s_agent_sandbox import SandboxdInClusterConnectionConfig

config = SandboxdInClusterConnectionConfig(
    mode="in-cluster-service",
    auth_token=os.environ["SANDBOXD_AUTH_TOKEN"],
)
```

Pass this config to `SandboxClient` or `AsyncSandboxClient`. The raw token must
be non-empty printable ASCII without spaces, matching TypeScript's `authToken`.
The SDK adds `Bearer ` to REST headers and all unary/streaming gRPC calls,
including after reconnect. Omit `auth_token` to preserve existing behavior.

This client-level credential is stored as `SecretStr` and hidden from config
repr and validation error messages. Configure the matching token on sandboxd
separately. It is independent of Kubernetes credentials and does not add TLS;
in-cluster traffic remains plaintext and requires appropriate network isolation.

### 9. sandboxd Router Tunnel Mode

Use `SandboxdRouterTunnelConnectionConfig` to reach sandboxd through
`sandbox-router-svc`. This supports Kata and gVisor Pods that cannot receive a
direct Pod port-forward. Install `k8s-agent-sandbox[grpc]` for the synchronous
client or `k8s-agent-sandbox[async,grpc]` for the asynchronous client.

The SDK opens one `kubectl port-forward` to the router's port 8080. REST requests
and gRPC calls share that tunnel. The SDK sets `X-Sandbox-ID`,
`X-Sandbox-Namespace`, and `X-Sandbox-Port` as HTTP headers or lowercase gRPC
metadata. `rest_port` and `grpc_port` select the sandboxd listeners, which default
to 8080 and 9090. gRPC requires sandbox-router v1.0.9 or later. The client omits
`X-Sandbox-Pod-IP` unless `pod_ip_routing` is true, so the router selects the
current Pod from its cache or DNS. Leave the default for `scoped-token`, which
rejects that header. Set `pod_ip_routing=True` only when the router cache is
disabled and the Sandbox has no Service.

```python
from k8s_agent_sandbox import SandboxClient, SandboxdRouterTunnelConnectionConfig

config = SandboxdRouterTunnelConnectionConfig(
    router_namespace="agent-sandbox-system",
)
with SandboxClient(connection_config=config) as client:
    sandbox = client.create_sandbox(warmpool="sandboxd-warmpool", namespace="default")
    sandbox.files.write("hello.txt", b"hello through the router\n")
    print(sandbox.commands.run("cat hello.txt").stdout)
```

`AsyncSandboxClient` accepts the same config and manages the tunnel without
blocking the event loop. `router_namespace` identifies the router's namespace,
independently of the Sandbox namespace. The router must be able to reach the
Sandbox on both sandboxd ports. The Kubernetes credentials must permit
port-forwarding to the router Pod through its Service.

This mode is opt-in. `SandboxdPodTunnelConnectionConfig` continues to connect
directly to the sandbox Pod. The router caps REST request timeouts at its proxy
timeout. When the router is not in `allow-all` mode, set `router_auth_token`.
The SDK sends that token as `Authorization` on REST requests and gRPC calls.
Use a Kubernetes bearer token for `tokenreview`, or a scoped token for
`scoped-token`. The router strips `Authorization`, so the SDK sends `auth_token`
in `X-Sandboxd-Token` instead. sandboxd accepts that header since
[#2164](https://github.com/kubernetes-sigs/agent-sandbox/pull/2164).

### 10. Labels and Pod Metadata

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

### 11. Deterministic, Retry-Safe Claim Creation

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

### 12. Custom Volume Claim Templates

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

### 13. Startup Latency: How the SDK Waits for Readiness

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

### 14. Targeting a Specific Cluster or Context

By default, `SandboxClient` and `AsyncSandboxClient` load their Kubernetes credentials via an in-cluster config if running inside a pod, otherwise `KUBECONFIG`, falling back to `~/.kube/config`'s `current-context` if unset. To target a different cluster/context instead, pass a pre-configured `api_client`.

> **Note:** The `kubectl` calls in the local-tunnel and sandboxd Pod/router tunnel modes target the same cluster as `api_client`. The SDK gives `kubectl` a short-lived kubeconfig built from the client's host, CA, client certificate and bearer token. Basic auth is not carried over.

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

### 15. Logging

The SDK logs through the `k8s_agent_sandbox` logger hierarchy and does not configure logging itself. To see its progress messages, configure logging in your application:

```python
import logging

logging.basicConfig(level=logging.INFO)
```

### 15. Streaming Exec and Cancellation (sandboxd runtime)

`sandbox.commands.run()` waits for the whole command to finish before returning
its output — fine for short one-shots, but not for long-running builds, hung
processes, or anything you might need to cancel from the outside. On the
**sandboxd runtime**, the SDK also exposes a streaming variant that yields
stdout/stderr in real time and lets you deliver POSIX signals to the running process through `commands.start()`.

Install the SDK with the `grpc` extra to enable it:

```bash
pip install "k8s-agent-sandbox[grpc]"
```

`sandbox.commands.start(command)` returns a `ProcessHandle` context manager.
Its `process_id` is set before `start()` returns, so you can signal the process
without first consuming an event. Iterate `handle.events()` for typed events
(`ProcessOutputEvent`, then a terminal `ProcessExitEvent`) and call
`handle.signal(sig)` to cancel:

```python
import sys
import signal as sig
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig

client = SandboxClient(connection_config=SandboxdPodTunnelConnectionConfig())
sandbox = client.create_sandbox(warmpool="sandboxd-warmpool", namespace="default")
try:
    with sandbox.commands.start("python long_task.py") as handle:
        for event in handle.events():
            if event.type in ("stdout", "stderr"):
                sys.stdout.buffer.write(event.data)
                sys.stdout.buffer.flush()
            elif event.type == "exit":
                print(f"exited with {event.exit_code}")
            if user_hit_ctrl_c():
                handle.signal(sig.SIGINT)
finally:
    sandbox.terminate()
```

`signal()` accepts either the cross-SDK name (`"SIGINT"`) or Python's own
`signal.SIGINT`. `env` and `cwd` work as they do on `run()`. Passing
`pty=PtySize(cols=120, rows=40)` (from `k8s_agent_sandbox.models`) allocates a
pseudo-terminal, which merges stderr into stdout.

The async client mirrors the interface via `async with` / `async for` /
`await`:

```python
async with await sandbox.commands.start("python long_task.py") as handle:
    async for event in handle.events():
        ...
    await handle.signal(sig.SIGINT)
```

`start()` is only available on the sandboxd runtime and raises
`NotImplementedError` on the legacy python-runtime connector, which does not
expose a streaming exec endpoint.

### 16. Batch claims

`SandboxClient.claim_batch()` claims a batch of `SandboxClaims` across one or more warm pools in one call,
returning a `SandboxBatch` handle right away. Each `BatchGroup` names a warm pool, how many claims to
create from it (`size`), and how many of them must become Ready for the group to count (`min_ready`,
which defaults to `size`). A group with `size=0` creates no claims and gets its members from `acquire()`,
and `wait_for_quorum()` and `group_outcomes()` skip it. The claims are named `<batch-id>-0` to `<batch-id>-<N-1>`, where `N` is the
sum of the group sizes. They carry the `agents.x-k8s.io/batch-id`, `agents.x-k8s.io/batch-lease-uid`, and `agents.x-k8s.io/batch-group`
labels (`batch-group` hashes the warm pool's name), and are created in the background, paced at `create_rps` creates per second with at most `max_in_flight` in flight.
Claims that a pool's Ready sandboxes cover, counted when `claim_batch()` checks the pool, skip `create_rps` pacing because adopting a Ready sandbox starts
no pod (the count is a snapshot, not a reservation), so only `max_in_flight` bounds them. When the watch observes a claim from a pool start cold before becoming Ready, the batch
zeroes that pool's credit and defers its next refresh. When the apiserver answers a create with 429, the batch pauses warm-pool
credit for numeric `Retry-After` values up to 30 seconds (with a 5-second minimum; missing or date-form headers use 5 seconds) and paces newly scheduled creates at `create_rps` until the cooldown ends. The batch
also has a `coordination.k8s.io/v1` Lease named `batch-<batch-id>`, which the handle renews while it is
alive. Every claim has an `ownerReference` to that Lease, so the Kubernetes garbage collector deletes the
claims once the Lease is gone. Deleting the Lease by hand (`kubectl delete lease batch-<batch-id>`) therefore
deletes the batch's claims too, in the background. `kubectl delete lease --cascade=orphan` instead keeps the
claims, which then stay until their `shutdownTime`.

```python
from k8s_agent_sandbox import BatchGroup, SandboxClient

client = SandboxClient()
batch = client.claim_batch(
    [BatchGroup(warmpool="python-sandbox-pool", size=8, min_ready=4)],
    namespace="default",
)

try:
    for group in batch.group_outcomes():
        if group.error is not None:
            print(f"{group.warmpool} failed: {group.error}")
            continue
        for member in group.members:
            batch.connect(member).commands.run("echo hello")
    if batch.err() is not None:
        print(f"batch stopped: {batch.err()}")
finally:
    batch.release()
```

`wait_for_quorum()` instead waits until every group has `min_ready` Ready members, so work starts with
the whole batch at once.

```python
from k8s_agent_sandbox import BatchEventType, BatchGroup, SandboxClient

client = SandboxClient()
batch = client.claim_batch(
    [BatchGroup(warmpool="python-sandbox-pool", size=8, min_ready=6)],
    namespace="default",
)

try:
    for member in batch.wait_for_quorum():
        batch.connect(member).commands.run("echo hello")
    # Members beyond the quorum are handed out as they become Ready, until the batch settles.
    for event in batch.events():
        if event.type is BatchEventType.MEMBER_READY:
            batch.connect(event.member).commands.run("echo hello")
finally:
    batch.release()
```

A batch's claims are consumed in one of three modes, fixed by whichever method is called first.

- `wait_for_quorum()` (quorum mode) waits until every group has `min_ready` Ready members and
  returns them in one list, each group's first `min_ready` in the order they became Ready, with the groups in
  batch order. It raises `QuorumUnreachableError` once any group has too few members that can still become
  Ready, and `BatchTimeoutError` if the quorum isn't reached within `quorum_timeout` of the batch's last claim
  create. After that no members are handed out and no more claims are created, and the same holds if the wait
  ends with any other exception, such as a cancellation or `KeyboardInterrupt`. It raises `BatchError` if the
  batch's watch or Lease renewal stops with an error while it waits, or if `release_group()` or
  `release_members()` releases a group with a `min_ready` above 0 from a reached quorum before it returns,
  and it can be called only once.
  If every group has `min_ready=0`, it returns an empty list at once. The handle checks the deadline only
  while a consumer waits or claims are still being created, so a quorum reached after the deadline, before any
  consumer checks it, still succeeds.
- `group_outcomes()` (group mode) yields one `GroupOutcome` per group, holding either its first
  `min_ready` Ready members, in the order they became Ready, or an `error`. A group fails with `QuorumUnreachableError` once
  too few of its members can still become Ready, and with `BatchTimeoutError` if it hasn't reached `min_ready`
  within `quorum_timeout` of the batch's last claim create. A group that `release_group()` or
  `release_members()` releases after it succeeds with members but before it is yielded fails with `BatchError`.
  A failed group is finished, so no more of its
  claims are created and none of its members are handed out. The handle checks the deadline only while a
  consumer waits or claims are still being created, so a group that reaches `min_ready` after the deadline,
  before any consumer checks it, still succeeds.
- `events()` yields `BatchEvent`s. It yields `MEMBER_READY` once per claim that becomes Ready, `MEMBER_FAILED`
  and `MEMBER_LOST` for claims that fail or are deleted.
  Called first (stream mode), it streams every Ready member. Called after `group_outcomes()`, it hands out a
  group's members only after that group has yielded successfully, starting with the Ready members beyond
  its first `min_ready` and then later ones as they become Ready. Called after `wait_for_quorum()`, it hands
  out the Ready members the quorum didn't return once the quorum is reached, and drops those still queued if a
  release fails the quorum before it returns. `group_outcomes()` and
  `wait_for_quorum()` can't be called after `events()` or after each other.

Both iterators can be called again to continue where they stopped. They also end early if the batch's
watch or Lease renewal stops with an error, so check `batch.err()` after the loop. `release()` deletes the
batch's claims and then its Lease; `detach(grace=None)` instead leaves everything in place and releases the
Lease so `get_batch()` can take the batch over.

Four methods free sandboxes before `release()`. `release_member(member)` deletes one member's claim.
`release_group(warmpool)` deletes every claim of a pool, pending ones included, for example a group that
`group_outcomes()` yields with an error, whose claims are otherwise kept until `release()`.
`release_members()` deletes every claim across all pools in the batch via label-scoped `deletecollection` while keeping the batch, its Lease, and its watch open for later `acquire()` calls.
`release_not_ready()` stops creating claims and deletes every initial claim that isn't Ready and was never
handed out, once you have what you need. Member deletes and the initial claims share `max_in_flight`, which
defaults to 20 on a handle from `get_batch()`. Released members are never handed out, are dropped from `members()`, and
count as unable to become Ready, so releasing members before a group or the quorum has been decided can make
it unreachable. Read a failed member's `reason` before releasing it. All four methods delete nothing once
this handle loses the batch's Lease or is detached, and raise `BatchError`. A watch error sets `err()` but
leaves the Lease held, so they still delete claims after one.

`acquire(warmpool, timeout=None, *, pod_annotations=None)` adds a member from any warm pool in the batch's namespace and blocks until
it is Ready, for example to replace a member released with `release_member()`. `acquire()` writes non-empty `pod_annotations` to `spec.additionalPodMetadata.annotations` so the controller propagates them onto the sandbox pod. `BatchGroup` has no pod annotation field, so callers that need per-pod annotations on every member pass `size=0` groups to `claim_batch()` and call `acquire()`. Its claim takes the next unused
ordinal, so ordinals are never reused, even across `detach()` and `get_batch()`. It isn't one of the initial
claims, so `events()`, the quorum consumers and `release_not_ready()` leave it alone, but `release_group()` on
its pool or `release_members()` releases it, and its `shutdownTime` counts from its own create. Its create follows the initial
claims' pacing, except that a create the pool's Ready sandboxes cover skips `create_rps` pacing. Once callers spend the
count from the pool's check and `create_rps` pacing would delay the next create, `acquire()` reads the pool again at most
once a second per pool (unless an expired `timeout` cuts a read short before recording it). When the watch observes a claim from a pool start cold before becoming Ready, the batch zeroes that
pool's credit and defers its next refresh. Its create and pool reads also share `max_in_flight` with
the initial claims and member deletes. While `release_group()` on its pool or `release_members()` is
deleting claims, `acquire()` waits for it to finish before creating its claim, since those deletes would
remove it too. `acquire()` raises `TerminalMemberError` if the create fails or the
member fails or is deleted before it is Ready, and `BatchTimeoutError` if checking an unchecked warm pool,
waiting for such a release, or getting the member Ready doesn't finish within `timeout`, which defaults to `quorum_timeout`. Both
subclass `BatchError`, which `acquire()` also raises directly when the handle stops; checking a pool not yet in the batch
can also raise `SandboxWarmPoolNotFoundError` or `SandboxTemplateNotFoundError` (both `SandboxError`) or a
Kubernetes API or network error. If it raises, including on a cancellation, `acquire()` drops the member from `members()` and deletes
its claim unless the claim is already gone, its create was rejected, or this handle lost the batch's Lease. `release_not_ready()`,
`release_members()`, and `release()` retry a failed delete, and calling `acquire()` again tries a new claim.

The `claim_batch()` defaults are `create_rps=50`, `max_in_flight=20`, `quorum_timeout=600` seconds,
`work_budget=3600` seconds, and `lease_duration=60` seconds. The client's Kubernetes API connection pool must hold
at least `max_in_flight + 2` connections so the batch's watch and Lease renewal each keep a slot alongside
in-flight requests. When a caller omits `max_in_flight` on a smaller pool of at least 3 connections,
`claim_batch()` clamps it down to `pool_size - 2` (a smaller pool raises `ValueError`). Pass `pool_size` to
`SandboxClient` or `AsyncSandboxClient` (or set `connection_pool_maxsize` on an injected `api_client`'s
configuration) to size the pool for a larger `max_in_flight` or multiple concurrent batches on one client. Every initial claim gets
the same `shutdownTime`, the time of the `claim_batch()` call plus `N / create_rps` seconds (rounded up) plus
`quorum_timeout` plus `work_budget` plus 600 seconds, so the controller deletes an abandoned batch's claims
eventually. `work_budget` is a hard cap. Nothing extends a claim's `shutdownTime`, so a batch held for a whole run
sets `work_budget` to the run's length.

The client tracks every batch handle it returns. `delete_all()` releases every tracked batch. The client's
exit cleanup (`cleanup=True`, or leaving a `with SandboxClient()` or `async with AsyncSandboxClient()`
block) releases the batches the client claimed with `claim_batch()`. Call `detach()` first to hand such a
batch off instead. Exit cleanup detaches a batch attached with `get_batch()` instead of releasing it, with
`detach()`'s default grace. Call `release()` to delete an attached batch. If a `release()` call fails,
client cleanup retries it. The `AsyncSandboxClient` atexit hook can't await `detach()`, so it leaves an
attached batch in place until its Lease goes stale.

`SandboxClient.get_batch()` attaches to an existing batch that was detached, returning a handle that
supports all of the above.

`SandboxBatch` (and its async twin `AsyncSandboxBatch`) expose:

- `batch_id`, `namespace`, `groups`, `size`: the batch's identity and its per-warmpool `BatchGroup`s.
- `members(warmpool=None)`: a snapshot of every `Member`, sorted by ordinal. A member's `state` is `PENDING`, `READY`, `FAILED` or `LOST`.
- `connect(member)`: a connected `Sandbox`/`AsyncSandbox` for a ready member.
- `wait_for_quorum()`, `group_outcomes()`, and `events()`, the three consumers described above.
- `err()`: the error that stopped the background watch/renewal, or `None`. Once it is set, no more claims
  are created.
- `lease_degraded()`: whether Lease renewal is failing, from the first failed renewal until one succeeds.
- `acquire(warmpool, timeout=None, *, pod_annotations=None)`, described above.
- `release_member(member)`, `release_group(warmpool)`, `release_members()`, and `release_not_ready()`, described above.
- `release()` deletes the batch's claims, then its Lease. Idempotent. It raises `BatchInUseError` and deletes
  nothing more if another handle holds the batch's Lease.
- `detach(grace=None)`: stops the background tasks and releases the Lease so another `get_batch` can take over; idempotent.

#### Error handling

Every batch-specific exception (`BatchTimeoutError`, `QuorumUnreachableError`, `TerminalMemberError`,
`BatchExistsError`, `BatchNotFoundError`, `BatchInUseError`, and `BatchLeaseExpiredError`) subclasses
`BatchError`, which subclasses `SandboxError`. Most callers catch `except BatchError` for batch
lifecycle, quorum, timeout, and member failures, catching a specific subclass only when branching on
that recovery path (such as `BatchTimeoutError` or `QuorumUnreachableError`). Catch `except SandboxError`
to also cover `SandboxWarmPoolNotFoundError` and `SandboxTemplateNotFoundError` when `claim_batch()` or
`acquire()` checks a pool, and `SandboxNotReadyError` on `connect()`. Bad arguments raise `ValueError`,
and direct Kubernetes API or network failures during setup, teardown, or checking an unchecked pool
propagate `ApiException` or transport errors unchanged, matching single-sandbox `SandboxClient` methods.

#### RBAC

For the batch operations, a batch driver needs, in the batch's namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sandbox-batch-driver
rules:
- apiGroups: ["extensions.agents.x-k8s.io"]
  resources: ["sandboxclaims"]
  verbs: ["create", "get", "list", "watch", "delete", "deletecollection"]
- apiGroups: ["extensions.agents.x-k8s.io"]
  resources: ["sandboxwarmpools", "sandboxtemplates"]
  verbs: ["get"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["create", "get", "update", "delete"]
```

Connecting to a member uses the same connection modes as a single sandbox, so it also needs the
access its mode needs, such as running `kubectl port-forward` in tunnel mode.

#### Batch reaper

A driver that dies without running any code (`SIGKILL`, an OOM kill, a lost node) never calls `release()`,
so its claims would hold their sandboxes until their `shutdownTime`. The batch reaper cleans up such
batches. Each run lists the batch Leases in its namespaces and picks the ones whose `renewTime` is more
than the Lease's duration plus `--skew-margin` in the past. Once that Lease's duration has passed since
the list, it deletes each picked Lease only if it hasn't changed since it was read, so a connected driver
whose clock runs behind either renews (failing the delete precondition with `409`) or stops locally before
the delete runs, and a stalled driver has already stopped locally before the delete while clock skew stays
within `--skew-margin` plus the Lease's duration (minus any in-flight request latency). Runs that find no
stale Leases exit without waiting, and each picked Lease waits only for its own duration so a long Lease
never holds back shorter ones. It deletes no claims itself. Each claim has an `ownerReference` to its
Lease, so the garbage collector deletes the claims, and their sandboxes, once the Lease is gone.

```bash
python -m k8s_agent_sandbox.batch_reaper --namespaces team-a,team-b
```

It takes `--namespaces` (comma-separated, defaulting to the reaper's own namespace, read from
`POD_NAMESPACE` or else the service account's namespace file), `--skew-margin`
(seconds, 60 by default and never below 5) and `--dry-run`, which logs the batches it would reap and
deletes nothing. It runs once and exits, logging one line per reaped batch, with a non-zero exit code if
any namespace or Lease failed, so it suits a CronJob that runs every one to two minutes with
`concurrencyPolicy: Forbid` and an `activeDeadlineSeconds` that covers the longest Lease duration wait
plus list and delete slack (for example `600` when running `300`-second Leases). A run cut short is
harmless, since every step is a delete and the next run starts over.

Bind the reaper's ServiceAccount to this ClusterRole with a RoleBinding in each namespace it reaps, never
a ClusterRoleBinding. RBAC can't limit `delete` to batch Leases, so the reaper limits itself to Leases
named `batch-<batch-id>` that carry that batch's `agents.x-k8s.io/batch-id` label.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: sandbox-batch-reaper
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["list", "get", "delete"]
```
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
