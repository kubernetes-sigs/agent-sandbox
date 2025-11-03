# Agentic Sandbox Python Client

This Python client provides a simple, high-level interface for creating and interacting with sandboxes managed by the Agent Sandbox controller. It's designed to be used as a context manager, ensuring that sandbox resources are properly created and cleaned up.

## How It Works

The `Sandbox` client automates the entire lifecycle of a temporary sandbox environment:

1.  **Initialization (`with Sandbox(...)`):** When you create a `Sandbox` instance within a `with` block, it initiates the process of creating a sandbox.
2.  **Claim Creation:** It creates a `SandboxClaim` Kubernetes resource. This claim tells the agent-sandbox controller to provision a new sandbox using a predefined `SandboxTemplate`.
3.  **Waiting for Readiness:** The client watches the Kubernetes API for the corresponding `Sandbox` resource to be created and become "Ready". This indicates that the pod is running and the server inside is active.
4.  **Port Forwarding:** Once the sandbox pod is ready, the client automatically starts a `kubectl port-forward` process in the background. This creates a secure tunnel from your local machine to the sandbox pod, allowing you to communicate with the server running inside.
5.  **Interaction:** The `Sandbox` object provides three main methods to interact with the running sandbox:
    *   `sandbox.run(command)`: Executes a shell command inside the sandbox.
    *   `sandbox.write(path, content)`: Uploads a file to the sandbox.
    *   `sandbox.read(path)`: Downloads a file from the sandbox.
6.  **Cleanup (`__exit__`):** When the `with` block is exited (either normally or due to an error), the client automatically cleans up all resources. It terminates the `kubectl port-forward` process and deletes the `SandboxClaim`, which in turn causes the controller to delete the `Sandbox` pod.

## Usage

### Prerequisites

- A running Kubernetes cluster (e.g., `kind`).
- The Agent Sandbox controller must be deployed with the extensions feature enabled.
- A `SandboxTemplate` resource must be created in the cluster.

### Client Initialization

The client is initialized with the name of the `SandboxTemplate` you want to use and the namespace where the resources should be created.

-   **`template_name` (str):** The name of the `SandboxTemplate` resource to use for creating the sandbox.
-   **`namespace` (str, optional):** The Kubernetes namespace to create the `SandboxClaim` in. Defaults to "default".

**Example:**

```python
from agentic_sandbox import Sandbox

with Sandbox(template_name="python-sandbox-template", namespace="default") as sandbox:
    result = sandbox.run("echo 'Hello, World!'")
    print(result.stdout)
```

## How to Test the Client

A test script, `test_client.py`, is included to verify the client's functionality.

### 1. Set Up the Environment

Before running the test, you need to set up the Kubernetes environment. From the root of the `agent-sandbox` repository, run:

```bash
# This command deploys the controller, creates the template, and builds/loads the image
make deploy-kind EXTENSIONS=true && \
kubectl apply -f examples/python-template-sandbox/sandbox-python-template.yaml && \
docker build -t sandbox-runtime:latest ./examples/python-template-sandbox --load && \
kind load docker-image sandbox-runtime:latest --name agent-sandbox
```

### 2. Install the Client

Install the client in editable mode. This allows you to make changes to the client and test them without reinstalling.

```bash
pip install -e .
```

### 3. Run the Test Script

Execute the `test_client.py` script. It will create a sandbox, run a command, perform file I/O, and then clean up the resources.

```bash
python test_client.py
```

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

### Installing the Package

You can install the client in a few ways:

1.  **From the local wheel file:**

    ```bash
    pip install dist/agentic_sandbox-0.1.0-py3-none-any.whl
    ```

2.  **Directly from the source in editable mode (for development):**

    ```bash
    pip install -e .
    ```


