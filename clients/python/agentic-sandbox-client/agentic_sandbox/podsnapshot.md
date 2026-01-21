# Agentic Sandbox Pod Snapshot Extension

This directory contains the Python client extension for interacting with the Agentic Sandbox to manage Pod Snapshots. This extension allows you to trigger checkpoints (snapshots) of a running sandbox and restore a new sandbox from the recently created snapshot.

## `podsnapshot.py`

This file defines the `PodSnapshotSandboxClient` class, which extends the base `SandboxClient` to provide snapshot capabilities.

### Key Features:

*   **`PodSnapshotSandboxClient(template_name: str, ...)`**:
    *   Initializes the client.
    *   If snapshot exists, the pod snapshot controller restores from the most recent snapshot matching the label of the `SandboxTemplate`, otherwise creates a new `Sandbox`.
*   **`snapshot_controller_ready(self) -> bool`**:
    *   Checks if the snapshot agent (both self-installed and GKE managed) is running and ready.
*   **`checkpoint(self, trigger_name: str) -> ExecutionResult`**:
    *   Triggers a manual snapshot of the current sandbox pod by creating a `PodSnapshotManualTrigger` resource.
    *   Waits for the snapshot to be processed.
    *   The pod snapshot controller creates a `PodSnapshot` resource automatically.
*   **`list_snapshots(self, policy_name: str, ready_only: bool) -> list`**:
    *   TBD
*   **`delete_snapshots(self, **filters) -> int`**:
    *   TBD
*   **Automatic Cleanup**:
    *   The `__exit__` method attempts to clean up triggers `PodSnapshotManualTrigger` created during the session, keeping the `PodSnapshot` resources alive after session exit.

## `test_podsnapshot_extension.py`

This file, located in the parent directory (`clients/python/agentic-sandbox-client/`), contains an integration test script for the `PodSnapshotSandboxClient` extension. It verifies the checkpoint and restore functionality.

### Test Phases:

1.  **Phase 1: Starting Counter & Checkpointing**:
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

3.  **Pod Snapshot Controller**: The Pod Snapshot controller must be installed in the standard cluster running inside gVisor(Userguide). The GCS bucket to store the pod snapshot states and respective permissions must be applied.
4.  **CRDs**: `PodSnapshotStorageConfig`, `PodSnapshotPolicy` CRDs must be applied. `PodSnapshotPolicy` should specify the selector match labels.
5.  **Sandbox Template**: A `SandboxTemplate` (e.g., `python-counter-template`) with runtime gVisor and label that matches that selector label in `PodSnapshotPolicy` must be available in the cluster.

### Running Tests:

To run the integration test, execute the script with the appropriate arguments:

```bash
python3 clients/python/agentic-sandbox-client/test_podsnapshot_extension.py \
  --labels app=agent-sandbox-workload \
  --template-name python-counter-template \
  --namespace sandbox-test
```

Adjust the `--namespace`, `--template-name`, and `--labels` as needed for your environment.