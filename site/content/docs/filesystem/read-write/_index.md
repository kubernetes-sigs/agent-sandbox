---
title: "Read and Write Files"
linkTitle: "Read and Write Files"
weight: 1
description: >
  Read file contents from and write files to the sandbox filesystem using the Python SDK.
---

{{% alert title="Prerequisite" color="info" %}}
These examples use a `SandboxTemplate` named `python-sandbox-template`. If it isn't installed in your cluster, `create_sandbox()` will return `NotFound`. See [Filesystem → Prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for a one-line install snippet.
{{% /alert %}}

## Write a File

Use `sandbox.files.write()` to create or overwrite a file inside the sandbox. The method accepts a path and content as either a string or bytes.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Write a text file (string content is automatically UTF-8 encoded)
sandbox.files.write("/home/user/greeting.txt", "Hello, world!")

# Write binary content
sandbox.files.write("/home/user/data.bin", b"\x00\x01\x02\x03")

sandbox.terminate()
```

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path in the sandbox filesystem |
| `content` | `str \| bytes` | — | File content. Strings are UTF-8 encoded automatically |
| `timeout` | `int` | `60` | Request timeout in seconds |

## Read a File

Use `sandbox.files.read()` to download a file's contents from the sandbox. The method returns raw `bytes`.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Read a text file
content = sandbox.files.read("/home/user/greeting.txt")
print(content.decode("utf-8"))  # 'Hello, world!'

# Read a binary file
data = sandbox.files.read("/home/user/data.bin")
print(data)  # b'\x00\x01\x02\x03'

sandbox.terminate()
```

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path to the file in the sandbox |
| `timeout` | `int` | `60` | Request timeout in seconds |

**Returns:** `bytes` — the raw file content.

## Write and Execute Code

A common pattern is writing a script to the sandbox and then executing it:

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Write a Python script
sandbox.files.write("/home/user/run.py", """
import json
data = {"result": 42, "status": "ok"}
print(json.dumps(data))
""")

# Execute it
result = sandbox.commands.run("python3 /home/user/run.py")
print(result.stdout)   # '{"result": 42, "status": "ok"}'
print(result.exit_code) # 0

sandbox.terminate()
```

## Async Usage

All file operations are available as async methods via `AsyncSandboxClient`:

```python
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig

async def main():
    config = SandboxDirectConnectionConfig(
        api_url="http://sandbox-router-svc.default.svc.cluster.local:8080"
    )
    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            template="python-sandbox-template", namespace="default"
        )
        await sandbox.files.write("/tmp/hello.txt", "Hello async!")
        content = await sandbox.files.read("/tmp/hello.txt")
        print(content.decode())

asyncio.run(main())
```
