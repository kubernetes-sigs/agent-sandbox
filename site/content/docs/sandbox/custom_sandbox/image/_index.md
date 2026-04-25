---
title: "Image"
linkTitle: "Image"
weight: 15
description: >
  Create a Sandbox with custom dependencies.
---
## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

## Architecture

The Agent Sandbox architecture separates the **Template** (the definition) from the **Claim** (your Python request) for a very specific reason: **speed**.
When a user applies a `SandboxTemplate` to a Kubernetes cluster, the controller typically spins up a `SandboxWarmPool`. These are pre-initialized, running pods that have already pulled your specific Docker image. When a `Python` script calls `client.create_sandbox("sandbox-template")`, it instantly grabs one of these pre-warmed pods.

## Workarounds

### 1. Install dependencies via sandbox.commands.run() function

```python
from k8s_agent_sandbox import SandboxClient
client = SandboxClient()
sandbox = client.create_sandbox("python-sandbox-template")
# Dynamically install a package before running your main logic
sandbox.commands.run("pip install custom-package==1.0.0")
response = sandbox.commands.run("python -c 'import custom_package; print(\"Success!\")'")
```

### \[NOT FOR PUBLISHING\] 2. Use `kubernetes` Python SDK to create a new SandboxTemplate instance

We can create a Python script that uses `kubernetes` SDK to deploy a custom template, or update an existing template with our custom Docker image.
