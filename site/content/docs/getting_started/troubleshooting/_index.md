---
title: "Troubleshooting"
linkTitle: "Troubleshooting"
weight: 20
url: "/docs/getting-started/troubleshooting/"
description: >
  How to troubleshoot in Agent Sandbox.
---
## Troubleshooting Agent Sandboxes

In complex agentic workflows, execution failures can happen at multiple layers—from missing dependencies in your Python environment to network timeouts and cluster-level resource exhaustion. 

While standard errors are often surfaced directly in your script, the `k8s_agent_sandbox` SDK provides specialized tools and methodologies to inspect, trace, and debug your sandbox environments effectively.

> Note: the source code can be found [here](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/site/content/docs/getting_started/troubleshooting/source)

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxWarmPool` named `python-sandbox-pool` (backed by a `SandboxTemplate` named `python-sandbox-template`) applied to your cluster. See the [Filesystem]({{< ref "/docs/filesystem" >}}) guide for a minimal `kubectl apply` example, or the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed with the tracing extra: `pip install "k8s-agent-sandbox[tracing]"`.

### SDK Logging

When you need granular visibility into the API calls the SDK is making to the Sandbox Router, configure Python logging to show the SDK's built-in log output. This is particularly useful when sandbox creation hangs or connection errors occur.

Example code:

```python
import logging
from k8s_agent_sandbox import SandboxClient

logging.basicConfig(level=logging.INFO)

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool")
payload = "echo 'Hello World!'"
response = sandbox.commands.run(payload)

print(response)
```

Example output:
```log
Creating SandboxClaim 'sandbox-claim-66ae1a5e' in namespace 'default' using warm pool 'python-sandbox-pool'...
2026-04-15 16:52:11,634 - INFO - Resolving sandbox name from claim 'sandbox-claim-66ae1a5e'...
2026-04-15 16:52:11,651 - INFO - Resolved sandbox name 'sandbox-claim-66ae1a5e' from claim status
2026-04-15 16:52:11,651 - INFO - Watching for Sandbox sandbox-claim-66ae1a5e to become ready...
2026-04-15 16:52:12,470 - INFO - Sandbox sandbox-claim-66ae1a5e is ready.
2026-04-15 16:52:12,470 - INFO - Starting tunnel for Sandbox sandbox-claim-66ae1a5e
2026-04-15 16:52:12,477 - INFO - Waiting for port-forwarding to be ready...
2026-04-15 16:52:12,983 - INFO - Tunnel ready at http://127.0.0.1:52403
stdout='Hello World!\n' stderr='' exit_code=0
2026-04-15 16:52:13,549 - INFO - Stopping port-forwarding for Sandbox sandbox-claim-66ae1a5e...
2026-04-15 16:52:13,553 - INFO - Connection to sandbox claim 'sandbox-claim-66ae1a5e' has been closed.
2026-04-15 16:52:13,564 - INFO - Terminated SandboxClaim: sandbox-claim-66ae1a5e
```

### Custom Sandbox Images and Output Inspection

Often, agent code fails because the environment lacks necessary system packages or dependencies. If your agent requires a specific setup, you should build a custom Docker image, push it to your registry, and reference it in your Kubernetes `SandboxTemplate`. You must also create a `SandboxWarmPool` that references that template before calling `create_sandbox(warmpool=...)`.

When executing commands inside this custom environment, the `response` object is your primary debugging tool. It strictly separates standard output, standard error, and the exit code.

#### Output Inspection Example

This example shows how to robustly check the execution results of a command, which is critical when validating custom Docker image behaviors.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()

# 1. Create a sandbox from a warm pool backed by a template with your custom Docker image
sandbox = client.create_sandbox(warmpool="python-sandbox-pool")

# 2. Run a command that might fail (e.g., executing a script with missing dependencies)
response = sandbox.commands.run("python3 /app/agent_script.py")

# 3. Inspect the execution results
if response.exit_code != 0:
    print(f"Execution Failed with exit code: {response.exit_code}")
    print(f"Error Details (stderr): {response.stderr}")
else:
    print(f"Execution Succeeded!")
    print(f"Output (stdout): {response.stdout}")

sandbox.terminate()
```

The output:
```log
Execution Failed with exit code: 2
Error Details (stderr): python3: can't open file '/app/agent_script.py': [Errno 2] No such file or directory
```

### Controller Log Level and Debug Flags

The Agent Sandbox Controller defaults to `info`-level logging. When debugging, you can increase verbosity or enable additional diagnostics by adding arguments to the controller's container spec:

```yaml
containers:
  - name: agent-sandbox-controller
    args:
      - --zap-log-level=debug
      - --enable-pprof-debug
      - --enable-tracing 
```

To view the controller logs after changing the log level:
```bash
kubectl logs -n agent-sandbox-system deployment/agent-sandbox-controller -f
```

### Infrastructure Diagnostics with kubectl

If the Python SDK is timing out before a sandbox is even returned, the issue is likely occurring at the cluster infrastructure layer. Because the SDK interacts with Kubernetes Custom Resource Definitions (CRDs) under the hood, `kubectl` is the best way to verify cluster state.

You can run the following commands in your terminal to diagnose the Sandbox Controller and Router.

#### Essential Diagnostic Commands

Check the status of your sandbox templates and warm pools to ensure your custom images are properly registered:
```bash
# Verify the template exists
kubectl get sandboxtemplates

# Verify the warm pool exists and has ready replicas
kubectl get sandboxwarmpools
kubectl describe sandboxwarmpool python-sandbox-pool
```

When `create_sandbox()` hangs, it usually means the controller cannot fulfill the claim (e.g., due to insufficient node resources or an exhausted warm pool). Inspect the claims:
```bash
# List all claims and check if any are stuck in "Pending"
kubectl get sandboxclaims

# Describe a specific pending claim to see event logs and errors
kubectl describe sandboxclaim <claim-name>
```

Finally, the Sandbox Router is responsible for translating the SDK's REST calls into cluster actions. Viewing its logs will reveal deeper backend issues:
```bash
# Locate the sandbox-router pod
kubectl get pods -n agent-sandbox-system | grep sandbox-router

# Tail the logs for errors
kubectl logs -n agent-sandbox-system <sandbox-router-pod-name> -f
```

### A Sandbox Stays `PodTerminating` After Its Node Is Gone

**Symptom.** The Sandbox reports `Ready=False` with reason `PodTerminating` for many minutes, and no replacement Pod appears. The Pod is still shown as `Running` (sometimes even `Ready`) with a `deletionTimestamp`, on a node that is `NotReady` or `Unknown`:

```bash
kubectl get sandbox <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'
kubectl get pod <pod-name> -o wide
kubectl get node <node-name>
```

**Cause.** Deleting a Pod is graceful: only the kubelet on the Pod's node can confirm that the containers stopped. If the node disappears while the Pod is still shutting down, nothing is left to confirm it, so the Pod object stays. A common way to get there is a Spot or preemptible node: the provider stops the machine after a short notice (2 minutes on AWS, 30 seconds on Google Cloud and Azure), while the Pod's `terminationGracePeriodSeconds` or a `preStop` hook (for example, one that waits for an agent to finish its turn) runs longer than that.

The controller keeps the Sandbox's Pod name, so it cannot create the replacement until the old Pod object is gone. This is deliberate: a second Pod could otherwise write to the same `ReadWriteOnce` volume while the first one may still be running.

The old Pod is removed only when the Node object is deleted (the Pod garbage collector then removes the Pod), when the node gets the `node.kubernetes.io/out-of-service` taint, or when someone force-deletes it. A node autoscaler that holds the Node with a finalizer until its drain finishes can wait for the whole grace period. For example, Karpenter waits until one minute past the Pod's `deletionTimestamp`.

**What to do.**

- On Spot or preemptible nodes, keep `terminationGracePeriodSeconds`, and any `preStop` wait, shorter than the provider's notice. A longer grace period cannot keep the work alive, because the machine is stopped anyway, and it delays the replacement.
- Keep the node autoscaler current. On AWS, Karpenter releases a Node at once when its instance no longer exists. Before karpenter-provider-aws v1.14.1, that check read a cache that never dropped terminated instances, so the Node was held for the full grace period.
- To recover now, once you have confirmed that the machine is gone, release the old Pod with one of:

  ```bash
  # Non-graceful node shutdown: the Pod is removed and its volumes detach
  kubectl taint node <node-name> node.kubernetes.io/out-of-service=nodeshutdown:NoExecute

  # Or force-delete the Pod
  kubectl delete pod <pod-name> --grace-period=0 --force
  ```

  Never do this while the machine might still be running: two Pods would then share the volume.
