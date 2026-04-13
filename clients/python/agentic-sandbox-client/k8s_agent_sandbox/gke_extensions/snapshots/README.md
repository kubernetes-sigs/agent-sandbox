# Agentic Sandbox Pod Snapshot Extension

This directory contains the Python client extension for interacting with the Agentic Sandbox to manage Pod Snapshots. This extension allows you to trigger snapshots of a running sandbox and restore a new sandbox from a recently created snapshot.

## Components

The snapshot functionality is driven by two main components:

### `PodSnapshotSandboxClient`
The main entry point for the snapshot extension. It inherits from the base `SandboxClient` but automatically validates that the required GKE Pod Snapshot CRDs are installed on the cluster upon initialization. It ensures that all sandboxes created via this client are instantiated as `SandboxWithSnapshotSupport`.

### `SandboxWithSnapshotSupport`
This class wraps the base `Sandbox` to seamlessly provide snapshot capabilities. It manages the sandbox lifecycle while granting access to the underlying snapshot operations via the `.snapshots` property.
*   **Suspend**: Scales the sandbox down to 0 replicas, temporarily pausing execution. It can optionally take a snapshot immediately before suspending (enabled by default).
*   **Resume**: Scales the sandbox back up to 1 replica, automatically restoring its state from the most recent available snapshot.
*   **Is Restored From Snapshot**: Checks if the current sandbox was successfully restored from a specific snapshot UID.
*   **Is Suspended**: Checks if the sandbox is currently suspended (i.e., scaled down to 0 replicas).

### `SnapshotEngine`
The core engine responsible for interacting with the GKE Pod Snapshot Controller.
*   **Create**: Creates `PodSnapshotManualTrigger` custom resources and waits for the snapshot to be completed.
*   **List**: Lists existing snapshots for a sandbox, with optional filtering by grouping labels and a flag to return ready-only snapshots.
*   **Delete**: Deletes a specific snapshot by UID.
*   **Delete All**: Deletes snapshots based on a strategy: either all snapshots for the pod, or filtered by grouping labels.
*   **Cleanup**: Ensures that manual trigger resources are cleanly deleted when the sandbox context exits.

## Usage Example

Here is an example demonstrating how to initialize the client and trigger a snapshot:

```python
from k8s_agent_sandbox.gke_extensions.snapshots import PodSnapshotSandboxClient

# Initialize the specialized snapshot client
client = PodSnapshotSandboxClient()

# Create a sandbox with snapshot capabilities enabled
sandbox = client.create_sandbox(
    template="python-counter-template", 
    namespace="default"
)

try:
    # Trigger a manual snapshot via the snapshots engine
    response = sandbox.snapshots.create("my-first-snapshot")

    if response.success:
        print(f"Snapshot created successfully! UID: {response.snapshot_uid}")
    else:
        print(f"Snapshot failed: {response.error_reason}")
        
    # Suspend the sandbox (automatically takes a snapshot and scales to 0 replicas)
    print("Suspending sandbox...")
    suspend_response = sandbox.suspend(snapshot_before_suspend=True)
    if suspend_response.success:
        print("Sandbox suspended successfully.")
        
    # Resume the sandbox (scales to 1 replica and restores from the latest snapshot)
    print("Resuming sandbox...")
    resume_response = sandbox.resume()
    if resume_response.success:
        print(f"Sandbox resumed! Restored from snapshot: {resume_response.restored_from_snapshot}")
finally:
    sandbox.terminate()
```

## `test_podsnapshot_extension.py`

This file, located in the parent directory (`clients/python/agentic-sandbox-client/`), contains an integration test script for the `PodSnapshotSandboxClient` extension. It verifies the snapshot and restore functionality.

### Test Phases:

1.  **Phase 1: Starting Counter Sandbox & Snapshotting**:
    *   Starts a sandbox with a counter application.
    *   Takes a snapshot (`test-snapshot-10`) after ~10 seconds.
    *   Takes a snapshot (`test-snapshot-20`) after ~20 seconds.
2.  **Phase 2: Restoring from Recent Snapshot**:
    *   Restores a sandbox from the second snapshot.
    *   Verifies that the sandbox has been restored from the recent snapshot. 

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

4.  **CRDs**: `PodSnapshotStorageConfig`, `PodSnapshotPolicy` CRDs must be applied. `PodSnapshotPolicy` should specify the selector match labels. (Note: For the test file to work, `maxSnapshotCountPerGroup` in `PodSnapshotPolicy` must be set to 2 or more, and the grouping labels must include `tenant-id` and `user-id`.)

5.  **Sandbox Template**: A `SandboxTemplate` (e.g., `python-counter-template`) with runtime gVisor, appropriate KSA and label that matches that selector label in `PodSnapshotPolicy` must be available in the cluster.

### Running Tests:

To run the integration test, execute the script with the appropriate arguments:

```bash
python3 clients/python/agentic-sandbox-client/test_podsnapshot_extension.py \
  --template-name python-counter-template \
  --namespace sandbox-test
```

Adjust the `--namespace`, `--template-name` as needed for your environment.
