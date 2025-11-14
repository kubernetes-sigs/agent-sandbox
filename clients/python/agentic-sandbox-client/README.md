# Agentic Sandbox Client Python

This Python client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It's designed to be used as a context manager,
ensuring that sandbox resources are properly created and cleaned up.

This client is designed for a scalable architecture where a central Kubernetes Gateway routes
traffic to a router pod, which then forwards requests to the correct sandbox based on an HTTP header.

## Usage

### Prerequisites

- A running Kubernetes cluster (e.g., Kind) with the Gateway API enabled.
- The Agent Sandbox controller must be deployed with the extensions feature enabled.
- The Sandbox Router and `Gateway` must be deployed in the cluster. This includes the router
  `Deployment` and `Service`, and the gateway `HTTPRoute` and `HealthCheckPolicy`. These can be
  found in the `clients/python/agentic-sandbox-client/sandbox_router/sandbox_router.yaml` config.
- A `SandboxTemplate` resource must be created in the cluster.
- The `kubectl` command-line tool must be installed and configured to connect to your cluster.

### Installation

1.  **Create a virtual environment:**
    ```bash
    python3 -m venv .venv
    ```
2.  **Activate the virtual environment:**
    ```bash
    source .venv/bin/activate
    ```
3.  **Option 1: Install from source via git:**

    ```bash
    # Replace "main" with a specific version tag (e.g., "v0.1.0") from
    # https://github.com/kubernetes-sigs/agent-sandbox/releases to pin a version tag.
    export VERSION="main"

    pip install "git+https://github.com/kubernetes-sigs/agent-sandbox.git@${VERSION}#subdirectory=clients/python/agentic-sandbox-client"
    ```

4.  **Option 2: Install from source in editable mode:**
    ```bash
    git clone https://github.com/kubernetes-sigs/agent-sandbox.git
    cd agent-sandbox/clients/agentic-sandbox-client-python
    cd ~/path_to_venv
    pip install -e .
    ```

### Example:

```python
from agentic_sandbox import SandboxClient

# The client will dynamically discover the IP of the 'external-http-gateway'
with SandboxClient(template_name="python-sandbox-template", namespace="default", gateway_name="external-http-gateway") as sandbox:
    result = sandbox.run("echo 'Hello, World!'")
    print(result.stdout)
```

## How It Works

The `SandboxClient` client automates the entire lifecycle of a temporary sandbox environment:

1. **Initialization (`with SandboxClient(...)`):** The client is initialized with the names of the
   `SandboxTemplate` and the `Gateway` you want to use.

- **`template_name` (str):** The name of the `SandboxTemplate` resource to use.
- **`namespace` (str, optional):** The Kubernetes namespace for all resources. Defaults to "default".
- **`gateway_name` (str, optional):** The name of the `Gateway` resource that provides the external
  entry point. The client will query this resource to find its public IP.

2. **Claim Creation:** It creates a `SandboxClaim` Kubernetes resource. This claim tells the
   agent-sandbox controller to provision a new sandbox pod and its associated headless service.

3. **Waiting for Readiness:** The client watches the Kubernetes API for two events:

- The `Sandbox` resource to become "Ready," indicating the pod is running and has passed its health checks.
- The `Gateway` resource to be assigned an external IP address by the cloud provider.

4. **Interaction via Gateway:** Once ready, the `SandboxClient` sends all requests to the discovered
   external IP of the `Gateway`. On each request, it adds an `X-Sandbox-ID` header containing the
   unique name of the `SandboxClaim`. The sandbox router pod in the cluster uses this header to
   forward the request to the correct sandbox pod.

5. **Interaction Methods:** The client provides three main methods:

- `run(command)`: Executes a shell command inside the sandbox.
- `write(path, content)`: Uploads a file to the sandbox.
- `read(path)`: Downloads a file from the sandbox.

6. **Cleanup (`__exit__`):** When the `with` block is exited (either normally or due to an error),
   the client automatically deletes the `SandboxClaim`, which in turn causes the controller to
   delete the `Sandbox` pod and its associated resources.

## How to Test the Client

A test script, `test_client.py`, is included to verify the client's functionality. You can run it
with the following command:

```bash
python test_client.py --gateway-name="external-http-gateway"`
```

You should see output indicating that the tests for command execution and file operations have passed.

## Packaging and Installation

This client is configured as a standard Python package using `pyproject.toml`.

### Prerequisites

- Python 3.7+
- `pip`
- `build` (install with `pip install build`)

### Building the Package

To build the package from the source, navigate to the `agentic-sandbox-client` directory and run
the following command:

```bash
python -m build
```

This will create a `dist` directory containing the packaged distributables: a `.whl` (wheel) file
and a `.tar.gz` (source archive).
