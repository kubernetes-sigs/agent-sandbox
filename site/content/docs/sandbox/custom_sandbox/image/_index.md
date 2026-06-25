---
title: "Image"
linkTitle: "Image"
weight: 15
description: >
  Create a Sandbox with custom dependencies.
---
## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxWarmPool` named `python-sandbox-pool` applied to your cluster. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

## Architecture

The Agent Sandbox architecture separates the **Template** (the definition) and **WarmPool** (pre-warmed instances of that template) from the **Claim** (your Python request) for a very specific reason: **speed**.
When a user applies a `SandboxTemplate` to a Kubernetes cluster, they typically also apply a `SandboxWarmPool` that references it. These are pre-initialized, running pods that have already pulled your specific Docker image. When a `Python` script calls `client.create_sandbox("sandbox-pool")`, it instantly grabs one of these pre-warmed pods.

## Workarounds

### 1. Install dependencies via sandbox.commands.run() function

```python
from k8s_agent_sandbox import SandboxClient
client = SandboxClient()
sandbox = client.create_sandbox("python-sandbox-pool")
# Dynamically install a package before running your main logic
sandbox.commands.run("pip install custom-package==1.0.0")
response = sandbox.commands.run("python -c 'import custom_package; print(\"Success!\")'")
```
