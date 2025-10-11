# Python Template Sandbox

This example demonstrates how to use `SandboxTemplate` and `SandboxClaim` to create a simple Python server in a sandbox container.
It includes a FastAPI server that can execute commands and a Python script to test it (`tester.py`).

## How it works

This example uses two key custom resources to create the sandbox: `SandboxTemplate` and `SandboxClaim`.

1.  **`SandboxTemplate` (`sandbox-python-template.yaml`):** This resource acts as a reusable blueprint for creating sandboxes. It defines the pod's specification, including the container image, ports, labels, and annotations. You can define multiple templates for different types of sandboxes.

2.  **`SandboxClaim` (`sandbox-python-claim.yaml`):** This resource is a request to create a new sandbox. It references a specific `SandboxTemplate` by name using the `templateRef` field. When a `SandboxClaim` is created, the `agent-sandbox-controller` (with extensions enabled) sees the claim and uses the referenced template to create a `Sandbox` resource. This `Sandbox` resource then manages the lifecycle of the actual sandbox pod.

This separation of concerns allows administrators to define a catalog of approved sandbox configurations (`SandboxTemplate`s) that users can then request on-demand (`SandboxClaim`s) without needing to know the full details of the pod specification.

## Labels

The `sandbox-python-template.yaml` defines the following labels for the sandbox pod:

-   `app: python-sandbox`: This label is used by the `run-test-kind.sh` script to identify the sandbox pod for port-forwarding and waiting for it to be ready.
-   `sandbox: codexec-python-sandbox`: This is a descriptive label that can be used to identify all sandboxes of this type.

These labels are important for selecting and managing the sandbox resources within the Kubernetes cluster.

## Testing on a local kind cluster using agent-sandbox

To test the sandbox on a local [kind](https://kind.sigs.k8s.io/) cluster, you can use the `run-test-kind.sh` script.
This script will:
1.  Create a kind cluster (if it doesn't exist).
2.  Build and deploy the agent sandbox controller to the cluster with the `--extensions` flag enabled. This is crucial for the `SandboxClaim` controller to be active.
3.  Build the python runtime sandbox image.
4.  Load the image into the kind cluster.
5.  Apply the `sandbox-python-template.yaml` and `sandbox-python-claim.yaml` manifests.
6.  Wait for the sandbox pod to be ready.
7.  Port-forward the sandbox pod's port 8888 to the local machine's port 8888.
8.  Run the `tester.py` script to test the sandbox.
9.  Clean up all the resources.

To run the script:
```bash
./run-test-kind.sh
```

## Python Classes in `main.py`

The `main.py` file defines the following Pydantic models to ensure type-safe data for the API endpoints:

### `ExecuteRequest`
This class models the request body for the `/execute` endpoint.
- **`command: str`**: The shell command to be executed in the sandbox.

### `ExecuteResponse`
This class models the response body for the `/execute` endpoint.
- **`stdout: str`**: The standard output from the executed command.
- **`stderr: str`**: The standard error from the executed command.
- **`exit_code: int`**: The exit code of the executed command.