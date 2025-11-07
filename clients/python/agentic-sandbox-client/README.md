# Agentic Sandbox Client Python

This Python client provides a simple, high-level interface for creating and interacting with sandboxes managed by the Agent Sandbox controller. It's designed to be used as a context manager, ensuring that sandbox resources are properly created and cleaned up.

## Usage

### Prerequisites

- A running Kubernetes cluster (e.g., `kind`).
- The Agent Sandbox controller must be deployed with the extensions feature enabled.
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
3. **Option 1: Install from source via git:**
    ```bash
    # Replace "main" with a specific version tag (e.g., "v0.1.0") from 
    # https://github.com/kubernetes-sigs/agent-sandbox/releases to pin a version tag.
    export VERSION="main"

    pip install "git+https://github.com/kubernetes-sigs/agent-sandbox.git@${VERSION}#subdirectory=clients/python/agentic-sandbox-client"
    ```
4. **Option 2: Install from source in editable mode:**
    ```bash
    git clone https://github.com/kubernetes-sigs/agent-sandbox.git
    cd agent-sandbox/clients/agentic-sandbox-client-python
    cd ~/path_to_venv
    pip install -e .
    ```

### Example:

```python
from agentic_sandbox import SandboxClient

with SandboxClient(template_name="sandbox-python-template", namespace="default") as sandbox:
    result = sandbox.run("echo 'Hello, World!'")
    print(result.stdout)
```

## How It Works

The `SandboxClient` client automates the entire lifecycle of a temporary sandbox environment:

1.  **Initialization (`with SandboxClient(...)`):** The client is initialized with the name of the `SandboxTemplate` you want to use and the namespace where the resources should be created:

    -   **`template_name` (str):** The name of the `SandboxTemplate` resource to use for creating the sandbox.
    -   **`namespace` (str, optional):** The Kubernetes namespace to create the `SandboxClaim` in. Defaults to "default". 
    -   When you create a `SandboxClient` instance within a `with` block, it initiates the process of creating a sandbox.
2.  **Claim Creation:** It creates a `SandboxClaim` Kubernetes resource. This claim tells the agent-sandbox controller to provision a new sandbox using a predefined `SandboxTemplate`.
3.  **Waiting for Readiness:** The client watches the Kubernetes API for the corresponding `Sandbox` resource to be created and become "Ready". This indicates that the pod is running and the server inside is active.
4.  **Port Forwarding:** Once the sandbox pod is ready, the client automatically starts a `kubectl port-forward` process in the background. This creates a secure tunnel from your local machine to the sandbox pod, allowing you to communicate with the server running inside.
5.  **Interaction:** The `SandboxClient` object provides three main methods to interact with the running sandbox:
    *   `run(command)`: Executes a shell command inside the sandbox.
    *   `write(path, content)`: Uploads a file to the sandbox.
    *   `read(path)`: Downloads a file from the sandbox.
6.  **Cleanup (`__exit__`):** When the `with` block is exited (either normally or due to an error), the client automatically cleans up all resources. It terminates the `kubectl port-forward` process and deletes the `SandboxClaim`, which in turn causes the controller to delete the `Sandbox` pod.


## How to Test the Client

A test script, `test_client.py`, is included to verify the client's functionality.
You should see output indicating that the tests for command execution and file operations have passed.

## Packaging and Installation

This client is configured as a standard Python package using `pyproject.toml`.

### Prerequisites

-   Python 3.7+
-   `pip`
-   `build` (install with `pip install build`)

### Building the Package

To build the package from the source, navigate to the `agentic-sandbox-client` directory and run the following command:

```bash
python -m build
```

This will create a `dist` directory containing the packaged distributables: a `.whl` (wheel) file and a `.tar.gz` (source archive).
