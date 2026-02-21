# Agentic Sandbox Pod Snapshot Extension

This directory contains the Python client extension for interacting with the Agentic Sandbox to manage Pod Snapshots. This extension allows you to trigger snapshots of a running sandbox and restore a new sandbox from the recently created snapshot.

## `podsnapshot_client.py`

This file defines the `PodSnapshotSandboxClient` class, which extend the base `SandboxClient` to provide snapshot capabilities.

### `PodSnapshotSandboxClient`

A specialized Sandbox client for interacting with the gke pod snapshot controller.

### Key Features:

*   **`PodSnapshotSandboxClient(template_name: str, podsnapshot_timeout: int = 180, server_port: int = 8080, ...)`**:
    *   Initializes the client with optional podsnapshot timeout and server port.
*   **`snapshot_controller_ready(self) -> bool`**:
    *   Checks if the snapshot agent (GKE managed) is running and ready.
*   **`__exit__(self)`**:
    *   Cleans up the `SandboxClaim` resources.

## `test_podsnapshot_extension.py`

This file, located in the parent directory (`clients/python/agentic-sandbox-client/`), contains an integration test script for the `PodSnapshotSandboxClient` extension. It verifies the snapshot and restore functionality.

### Test Phases:

1.  **Phase 1: Starting Counter Sandbox**:
    *   Starts a sandbox with a counter application.

### Prerequisites

1.  **Python Virtual Environment**:
    ```bash
    python3 -m venv .venv
    source .venv/bin/activate
    ```

2.  **Install Dependencies**:
    ```bash
    pip install kubernetes
    pip install -e clients/python/agentic-sandbox-client/
    ```

3.  **Pod Snapshot Controller**: The Pod Snapshot controller must be installed in a **GKE standard cluster** running with **gVisor**. 
   * For detailed setup instructions, refer to the [GKE Pod Snapshots public documentation](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots).
   * Ensure a GCS bucket is configured to store the pod snapshot states and that the necessary IAM permissions are applied.

4.  **CRDs**: `PodSnapshotStorageConfig`, `PodSnapshotPolicy` CRDs must be applied. `PodSnapshotPolicy` should specify the selector match labels.

5.  **Sandbox Template**: A `SandboxTemplate` (e.g., `python-counter-template`) with runtime gVisor, appropriate KSA and label that matches that selector label in `PodSnapshotPolicy` must be available in the cluster.

### Running Tests:

To run the integration test, execute the script with the appropriate arguments:

```bash
python3 clients/python/agentic-sandbox-client/test_podsnapshot_extension.py \
  --labels app=agent-sandbox-workload \
  --template-name python-counter-template \
  --namespace sandbox-test
```

Adjust the `--namespace`, `--template-name`, and `--labels` as needed for your environment.