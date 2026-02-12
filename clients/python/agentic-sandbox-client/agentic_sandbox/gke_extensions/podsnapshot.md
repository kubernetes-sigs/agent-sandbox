# Agentic Sandbox Pod Snapshot Extension

This directory contains the Python client extension for interacting with the Agentic Sandbox to manage Pod Snapshots. This extension allows you to trigger snapshots of a running sandbox and restore a new sandbox from the recently created snapshot.

## `podsnapshot_client.py`

This file defines the `SnapshotPersistenceManager` and `PodSnapshotSandboxClient` class, which extend the base `SandboxClient` to provide snapshot capabilities.

### `SnapshotPersistenceManager`

A utility class for managing local persistence of snapshot metadata in a secure directory. Stores metadata as a dictionary keyed by `trigger_name`.

### `PodSnapshotSandboxClient`

A specialized Sandbox client for interacting with the gke pod snapshot controller.

### Key Features:

*   **`PodSnapshotSandboxClient(template_name: str, podsnapshot_timeout: int = 180, server_port: int = 8080, ...)`**:
    *   Initializes the client with optional podsnapshot timeout and server port.
    *   If snapshot exists, the pod snapshot controller restores from the most recent snapshot matching the label of the `SandboxTemplate`, otherwise creates a new `Sandbox`.
*   **`snapshot_controller_ready(self) -> bool`**:
    *   Checks if the snapshot agent (both self-installed and GKE managed) is running and ready.
*   **`snapshot(self, trigger_name: str) -> SnapshotResponse`**:
    *   Triggers a manual snapshot of the current sandbox pod by creating a `PodSnapshotManualTrigger` resource.
    *   The trigger_name is suffixed with unique hash.
    *   Waits for the snapshot to be processed.
    *   The pod snapshot controller creates a `PodSnapshot` resource automatically.
    *   Returns the SnapshotResponse object(success, error_code, error_reason, trigger_name).
*   **`list_snapshots(self, policy_name: str, ready_only: bool = True) -> list | None`**:
    *  TBD
*   **`delete_snapshots(self, trigger_name: str) -> int`**:
    *  TBD
*   **Automatic Cleanup**:
    *   The `__exit__` method cleans up the `SandboxClaim` resources.

## `test_podsnapshot_extension.py`

This file, located in the parent directory (`clients/python/agentic-sandbox-client/`), contains an integration test script for the `PodSnapshotSandboxClient` extension. It verifies the snapshot and restore functionality.

### Test Phases:

1.  **Phase 1: Starting Counter & Snapshotting**:
    *   Starts a sandbox with a counter application.
    *   Takes a snapshot (`test-snapshot-10`) after ~10 seconds.
    *   Takes a second snapshot (`test-snapshot-20`) after another ~10 seconds.
2.  **Phase 2: Restoring from Recent Snapshot**:
    *   Restores a sandbox from the second snapshot.
    *   Verifies that the counter continues from where it left off (>= 20), proving the state was preserved.

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