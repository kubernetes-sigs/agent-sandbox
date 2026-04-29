---
title: "Agent Sandbox Shutdown Time"
linkTitle: "Agent Sandbox Shutdown Time"
weight: 2
description: >
  Set up a specific time when the Sandbox must be deleted.
---
## Sandbox Expiration

In many agentic workflows, you don't need a sandbox running indefinitely. To prevent resource leaks, runaway tasks, or unbounded compute costs, you need a way to ensure that a session is automatically terminated after a specific deadline.

While standard sandboxes run until manually deleted, configuring a `shutdownTime` allows you to schedule an exact expiration timestamp. Once this timestamp is reached, the sandbox and its associated resources are automatically garbage-collected by the control plane.

## Prerequisites

This guide uses `kubectl` directly and is compatible with any Kubernetes environment (KinD, Minikube, Docker Desktop, GKE, etc.).

- A running Kubernetes cluster.
- The [`kubectl`](https://kubernetes.io/docs/tasks/tools/#kubectl) CLI tool installed and configured to point to your cluster.
- The [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- A `SandboxTemplate` named `simple-sandbox-template` applied to your cluster. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

### Scheduled Shutdown

Unlike manual termination, setting a `shutdownTime` provides a guaranteed, hard deadline for the sandbox's lifecycle. This is ideal for ephemeral CI/CD test runs, untrusted code execution with strict timeouts, or simple cost-control mechanisms.

#### Basic Workflow Example with kubectl

The following example demonstrates how to define a sandbox claim with an explicit `shutdownTime`, apply it directly to your cluster using `kubectl`, and verify the scheduled cleanup.

Define the shutdown time (in this example it's the current time plus 1 minute):

```bash
SHUTDOWN_TIME=$(date -u -d "+1 minute" +%Y-%m-%dT%H:%M:%SZ)
```

Apply an example sandbox with the `shutdownPolicy` and `shutdownTime`:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: dynamic-ephemeral-sandbox
spec:
  replicas: 1
  shutdownPolicy: Delete
  shutdownTime: "${SHUTDOWN_TIME}"
  podTemplate:
    spec:
      containers:
      - name: workspace
        image: alpine:latest
        command: ["sleep", "infinity"]
EOF
```

Verify that the sandbox is deleted:

```bash
kubectl get sandbox dynamic-ephemeral-sandbox
sleep 60
kubectl get sandbox dynamic-ephemeral-sandbox
```

#### Basic Workflow Example with Python SDK

When creating a new sandbox via the `k8s_agent_sandbox` SDK, you can customize its readiness checks and lifecycle behavior using optional parameters:

* **`sandbox_ready_timeout`**: The maximum time (in seconds) the client will wait for the sandbox environment to become ready before timing out.
* **`shutdown_after_seconds`**: A Time-To-Live (TTL) integer in seconds. Setting this parameter tells the SDK to automatically populate the underlying Kubernetes claim's `spec.lifecycle` with a `shutdownPolicy` of `"Delete"` and schedule the deletion for *now + shutdown_after_seconds* (UTC). 

The following example demonstrates how to pass these parameters. Notice how the SDK handles the cluster cleanup policy for you:

```python
import time
from k8s_agent_sandbox import SandboxClient

def verify_sandbox_lifecycle():
    client = SandboxClient()
    ttl_seconds = 5

    print(f"Creating sandbox with a {ttl_seconds}-second TTL...")

    # 1. Verify creation and sandbox_ready_timeout
    # If the sandbox doesn't become ready within 15 seconds, this will raise an error.
    sandbox = client.create_sandbox(
        "simple-sandbox-template",
        sandbox_ready_timeout=15,
        shutdown_after_seconds=ttl_seconds
    )

    print("Sandbox created successfully! Running initial command...")
    response = sandbox.commands.run("echo 'Sandbox is alive!'")
    print(f"Output: {response}\n")

    # 2. Verify shutdown_after_seconds (Auto-deletion)
    wait_time = ttl_seconds + 3  # Add a small buffer for the Kubernetes controller sync
    print(f"Waiting {wait_time} seconds for the cluster to auto-delete the sandbox...")
    time.sleep(wait_time)

    print("Attempting to run a command on the expired sandbox...")
    try:
        # This should fail because the shutdownPolicy: "Delete" was triggered by the cluster
        sandbox.commands.run("echo 'Is anyone there?'")
        print("❌ FAILED: Sandbox is still alive! The shutdown policy did not trigger.")
    except Exception as e:
        print(f"✅ SUCCESS: Sandbox is no longer accessible! The cluster cleaned it up.")
        print(f"   Error received: {e}")

if __name__ == "__main__":
    verify_sandbox_lifecycle()
```
