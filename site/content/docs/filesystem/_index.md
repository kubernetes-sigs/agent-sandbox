---
title: "Filesystem"
linkTitle: "Filesystem"
weight: 15
description: >
  Read, write, list, and transfer files inside sandboxes using the Python SDK.
---

The Agent Sandbox Python SDK (`k8s-agent-sandbox`) provides a `files` API on every sandbox instance for interacting with the sandbox filesystem. You can read and write files, list directories, check if paths exist, and upload or download data — all through the SDK without needing `kubectl exec`.

All file operations are also available as async methods via `AsyncSandboxClient`.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. A `SandboxTemplate` defines the pod spec (image, resources, probes, optional `runtimeClassName` for gVisor/Kata) used when a sandbox is created. It must exist in the target namespace before `create_sandbox(template=...)` will succeed — otherwise the call returns a `NotFound` error.

Apply this minimal template once per namespace:

```bash
kubectl apply -n default -f - <<'EOF'
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-sandbox-template
spec:
  podTemplate:
    spec:
      containers:
      - name: python-runtime
        image: us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/python-runtime-sandbox:latest-main
        ports:
        - containerPort: 8888
        readinessProbe:
          httpGet: { path: "/", port: 8888 }
          periodSeconds: 1
      restartPolicy: OnFailure
EOF
```

The full template (with isolation runtime options) lives at [`clients/python/agentic-sandbox-client/python-sandbox-template.yaml`](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/python-sandbox-template.yaml) in the repository.

## Connect to a sandbox

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")
```

## Write a file

```python
sandbox.files.write("/home/user/hello.txt", "Hello from the SDK!")
```

## Read a file

```python
content = sandbox.files.read("/home/user/hello.txt")
print(content.decode())  # 'Hello from the SDK!'
```

## List directory contents

```python
entries = sandbox.files.list("/home/user")
for entry in entries:
    print(f"{entry.name} ({entry.type}, {entry.size} bytes)")
```

## Check if a path exists

```python
if sandbox.files.exists("/home/user/hello.txt"):
    print("File exists!")
```

## Clean up

```python
sandbox.terminate()
```
