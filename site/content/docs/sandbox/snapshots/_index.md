---
title: "Snapshots"
linkTitle: "Snapshots"
weight: 15
description: >
  Create a Sandbox and optimize the GKE cluster resource usage without losing the session data in your Sandbox.
---
## Sandbox Snapshots

In many agentic workflows, you don't need a sandbox running indefinitely, but you need to preserve the exact state of a session—including filesystem changes and memory state—to resume it later.

While standard sandboxes are ephemeral, the `PodSnapshotSandboxClient` allows you to manually "freeze" a gVisor-protected sandbox and rehydrate that state into a new instance later.

## Prerequisites

This guide requires a GKE Autopilot cluster with a gVisor node pool. See [GKE Cluster Setup](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots) for infrastructure setup instructions.

- A GKE Autopilot cluster with a gVisor node pool and necessary CRDs applied.
- Google Cloud credentials configured in your environment.
- The [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxTemplate` named `simple-sandbox-template` applied to your cluster. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

### Manual Snapshot & Restore

Unlike automatic pausing, snapshots give you granular control over when state is saved. This is ideal for multi-turn agents where the environment needs to be "parked" between user prompts to save costs.

#### Basic Workflow Example

The following example demonstrates creating a sandbox, modifying its filesystem, taking a snapshot, and restoring that state into a completely new sandbox.

> Note: this example uses `simple-sandbox-template`, which you should create in your GKE cluster first. The associated resources can be found [here](https://github.com/volatilemolotov/agent-sandbox/tree/main/site/content/docs/sandbox/snapshots/source).


```python
import time
from k8s_agent_sandbox.gke_extensions.snapshots import PodSnapshotSandboxClient

SLEEP_TIME = 10
def sleep():
    print(f"sleep {SLEEP_TIME} sec.")
    time.sleep(SLEEP_TIME)

# 1. Initialize the snapshot-capable client
client = PodSnapshotSandboxClient()

# 2. Create the sandbox
sandbox = client.create_sandbox("simple-sandbox-template")
print(sandbox)

# 3. Run a command that alters the filesystem (e.g., Playwright caching data)
response = sandbox.commands.run("mkdir -p /tmp/data && echo 'session_active' > /tmp/data/status.txt")
print(response)
sleep()

# 4. Snapshot the Sandbox
# This freezes the gVisor container state.
snapshot_response = sandbox.snapshots.create("my-trigger")
sleep()
assert snapshot_response is not None

print(f"Snapshot saved with ID: {snapshot_response.snapshot_uid}")

# 5. Clean up the original sandbox
sandbox.terminate()
sleep()

# 6. Later, restore the sandbox from the snapshot
restored_sandbox = client.create_sandbox("simple-sandbox-template")
is_restored = restored_sandbox.is_restored_from_snapshot(snapshot_response.snapshot_uid)
print(f"Is restored?\nAnswer: {is_restored}")

# 7. Verify the filesystem state was preserved
response = restored_sandbox.commands.run("cat /tmp/data/status.txt")
print(response.stdout) # Should output 'session_active'
```
