# Agentic Sandbox Pod Snapshot Extension

This directory contains the Python client extension for interacting with the Agentic Sandbox to manage Pod Snapshots. This extension allows you to trigger snapshots of a running sandbox and restore a new sandbox from the recently created snapshot.

## `podsnapshot_client.py`

This file defines the `PodSnapshotSandboxClient` class, which extends the base `SandboxClient` to provide snapshot capabilities.

### `PodSnapshotSandboxClient`

A specialized Sandbox client for interacting with the GKE Pod Snapshot Controller.

### Key Features:

*   **`PodSnapshotSandboxClient(template_name: str, podsnapshot_timeout: int = 180, ...)`**:
    *   Initializes the client with optional podsnapshot timeout.
    *   If snapshot exists, the pod snapshot controller restores from the most recent snapshot matching the label of the `SandboxTemplate`, otherwise creates a fresh `Sandbox`.
*   **`snapshot(self, trigger_name: str) -> SnapshotResponse`**:
    *   Triggers a manual snapshot of the current sandbox pod by creating a `PodSnapshotManualTrigger` resource.
    *   The `trigger_name` is suffixed with a timestamp and unique hash.
    *   Waits for the snapshot to be processed.
    *   The Pod Snapshot Controller creates a `PodSnapshot` resource automatically.
    *   Returns the SnapshotResponse object(success, error_code, error_reason, trigger_name, snapshot_uid).
*   **`is_restored_from_snapshot(self, snapshot_uid: str) -> RestoreCheckResult`**:
    *   Checks if the sandbox pod was restored from the specified snapshot.
    *   Verifies restoration by checking the 'PodRestored' condition in the pod status and confirming the message contains the expected snapshot UID.
    *   Returns RestoreCheckResult object(success, error_code, error_reason).
*   **`list_snapshots(self, grouping_labels: dict[str, str] | None = None, ready_only: bool = True) -> ListSnapshotResult`**:
    *   Checks for existing snapshots matching the provided grouping labels associated with the sandbox.
    *   If `ready_only` is True, only returns snapshots that are in the 'Ready' state.
    *   Returns a `ListSnapshotResult` object (success, error_code, error_reason, snapshots) containing the mapped snapshots sorted by creation timestamp (newest first).
*   **`delete_snapshots(self, grouping_labels: dict[str, str] | None = None, snapshot_uid: str | None = None) -> DeleteSnapshotResult`**:
    *   Deletes snapshots based on provided parameters: a specific snapshot if `snapshot_uid` is provided, all matching `grouping_labels`, or all snapshots for the pod if neither is provided.
    *   Returns a `DeleteSnapshotResult` object (success, error_code, error_reason, deleted_snapshots) containing the list of successfully deleted snapshot UIDs.
*   **`__exit__(self)`**:
    *   Cleans up the `PodSnapshotManualTrigger` resources.
    *   Cleans up the `SandboxClaim` resources.

## `test_podsnapshot_extension.py`

This file, located in the parent directory (`clients/python/agentic-sandbox-client/`), contains an integration test script for the `PodSnapshotSandboxClient` extension. It verifies the snapshot and restore functionality.

### Test Phases:

1.  **Phase 1: Starting Counter Sandbox & Snapshotting**:
    *   Starts a sandbox with a counter application.
    *   Takes a snapshot (`test-snapshot-10`) after ~10 seconds.
    *   Takes a snapshot (`test-snapshot-20`) after ~20 seconds.
2.  **Phase 2: Restoring from Recent Snapshot**:
    *   Restores a sandbox from the second snapshot.
    *   Verifies that sandbox has been restored from the recent snapshot. 

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
  --template-name python-counter-template \
  --namespace sandbox-test
```

Adjust the `--namespace`, `--template-name` as needed for your environment.
