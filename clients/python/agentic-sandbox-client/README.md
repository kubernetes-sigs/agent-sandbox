# Agentic Sandbox Client Python

This Python client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It uses a **Resource Handle** pattern,
allowing for persistent connections to stateful environments.

It supports a **scalable, cloud-native architecture** using Kubernetes Gateways and a specialized
Router, while maintaining a convenient **Developer Mode** for local testing.

## Architecture

The client operates in two modes:

1.  **Production (Gateway Mode):** Traffic flows from the Client -> Cloud Load Balancer (Gateway)
    -> Router Service -> Sandbox Pod. This supports high-scale deployments.
2.  **Development (Tunnel Mode):** Traffic flows from Localhost -> `kubectl port-forward` -> Router
    Service -> Sandbox Pod. This requires no public IP and works on Kind/Minikube.
3.  **Advanced / Internal Mode**: The client connects directly to a provided api_url, bypassing
    discovery. This is useful for in-cluster communication or when connecting through a custom domain.

## Prerequisites

- A running Kubernetes cluster.
- The **Agent Sandbox Controller** installed.
- `kubectl` installed and configured locally.

## Setup: Deploying the Router

Before using the client, you must deploy the `sandbox-router`. This is a one-time setup.

1.  **Build and Push the Router Image:**

    For both Gateway Mode and Tunnel mode, follow the instructions in [sandbox-router](sandbox-router/README.md)
    to build, push, and apply the router image and resources.

2.  **Create a Sandbox Template:**

    Ensure a `SandboxTemplate` exists in your target namespace. The [test_client.py](test_client.py)
    uses the [python-runtime-sandbox](../../../examples/python-runtime-sandbox/) image.

    ```bash
    kubectl apply -f python-sandbox-template.yaml
    ```

## Installation

1.  **Create a virtual environment:**

    ```bash
    python3 -m venv .venv
    source .venv/bin/activate
    ```

2.  **Option 1: Install from PyPI (Recommended):**

    The package is available on [PyPI](https://pypi.org/project/k8s-agent-sandbox/) as `k8s-agent-sandbox`.

    ```bash
    pip install k8s-agent-sandbox
    ```

    If you are using [tracing with GCP](GCP.md#tracing-with-open-telemetry-and-google-cloud-trace),
    install with the optional tracing dependencies:

    ```bash
    pip install "k8s-agent-sandbox[tracing]"
    ```

3.  **Option 2: Install from source via git:**

    ```bash
    # Replace "main" with a specific version tag (e.g., "v0.1.0") from
    # https://github.com/kubernetes-sigs/agent-sandbox/releases to pin a version tag.
    export VERSION="main"

    pip install "git+https://github.com/kubernetes-sigs/agent-sandbox.git@${VERSION}#subdirectory=clients/python/agentic-sandbox-client"
    ```

4.  **Option 3: Install from source in editable mode:**

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

    ```
    pip install -e ".[tracing]"
    ```

## Usage Examples

### 1. Production Mode (GKE Gateway)

Use this when running against a real cluster with a public Gateway IP. The client automatically
discovers the Gateway.

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxRouterConfig

# Configure connection via the GKE Gateway
config = SandboxRouterConfig(
    gateway_name="external-http-gateway",
    gateway_namespace="default"
)

client = SandboxClient(config=config)

# Create and use the sandbox
sandbox = client.create_sandbox("python-sandbox-template", namespace="default")
try:
    print(sandbox.core.run("echo 'Hello from Cloud!'").stdout)
finally:
    sandbox.terminate()
```

### 2. Developer Mode (Local Tunnel)

Use this for local development or CI. If you omit `gateway_name`, the client automatically opens a
secure tunnel to the Router Service using `kubectl`.

```python
from k8s_agent_sandbox import SandboxClient

# Default config uses local port-forwarding (Developer Mode)
client = SandboxClient()

sandbox = client.create_sandbox("python-sandbox-template", namespace="default")
try:
    print(sandbox.core.run("echo 'Hello from Local!'").stdout)
finally:
    sandbox.terminate()
```

## Testing

A test script is included to verify the full lifecycle (Creation -> Execution -> File I/O -> Cleanup).

### Run in Dev Mode:

```
python test_client.py --namespace default
```

### Run in Production Mode:

```
python test_client.py --gateway-name external-http-gateway
```
