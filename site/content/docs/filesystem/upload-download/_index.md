---
title: "Upload and Download"
linkTitle: "Upload and Download"
weight: 3
description: >
  Transfer files between your local machine and the sandbox filesystem.
---

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## Upload a Local File

Use `sandbox.files.write()` with the contents of a local file to upload it to the sandbox.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Upload a local file to the sandbox
with open("local_data.csv", "rb") as f:
    sandbox.files.write("/home/user/data.csv", f.read())

# Verify the upload
entries = sandbox.files.list("/home/user")
for entry in entries:
    print(f"{entry.name} ({entry.size} bytes)")

sandbox.terminate()
```

## Download a File from the Sandbox

Use `sandbox.files.read()` to download a file and write it locally.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Generate a file inside the sandbox
sandbox.commands.run("echo 'analysis complete' > /home/user/results.txt")

# Download it to the local machine
content = sandbox.files.read("/home/user/results.txt")
with open("downloaded_results.txt", "wb") as f:
    f.write(content)

print(f"Downloaded {len(content)} bytes")

sandbox.terminate()
```

## Upload a Directory

Upload all files from a local directory by iterating over its contents:

```python
import os
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

local_dir = "./my_project"
sandbox_dir = "/home/user/project"

for filename in os.listdir(local_dir):
    filepath = os.path.join(local_dir, filename)
    if os.path.isfile(filepath):
        with open(filepath, "rb") as f:
            sandbox.files.write(f"{sandbox_dir}/{filename}", f.read())
        print(f"Uploaded {filename}")

# Verify
entries = sandbox.files.list(sandbox_dir)
print(f"\n{len(entries)} files in sandbox:")
for entry in entries:
    print(f"  {entry.name} ({entry.size} bytes)")

sandbox.terminate()
```

## Download Multiple Files

Download all files from a sandbox directory:

```python
import os
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

sandbox_dir = "/home/user/output"
local_dir = "./downloaded_output"
os.makedirs(local_dir, exist_ok=True)

entries = sandbox.files.list(sandbox_dir)
for entry in entries:
    if entry.type == "file":
        content = sandbox.files.read(f"{sandbox_dir}/{entry.name}")
        with open(os.path.join(local_dir, entry.name), "wb") as f:
            f.write(content)
        print(f"Downloaded {entry.name} ({entry.size} bytes)")

sandbox.terminate()
```

## End-to-End Example: Upload, Process, Download

A common pattern is uploading data, processing it inside the sandbox, and downloading the results:

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# 1. Upload input data
with open("input.csv", "rb") as f:
    sandbox.files.write("/home/user/input.csv", f.read())

# 2. Upload a processing script
sandbox.files.write("/home/user/process.py", """
import csv
with open('/home/user/input.csv') as f:
    reader = csv.reader(f)
    rows = list(reader)
print(f"Processed {len(rows)} rows")
with open('/home/user/output.csv', 'w') as f:
    writer = csv.writer(f)
    writer.writerows(rows)
""")

# 3. Run the script
result = sandbox.commands.run("python3 /home/user/process.py")
print(result.stdout)

# 4. Download the output
output = sandbox.files.read("/home/user/output.csv")
with open("output.csv", "wb") as f:
    f.write(output)

sandbox.terminate()
```
