---
title: "List Files and Directories"
linkTitle: "List Files and Directories"
weight: 2
description: >
  List directory contents and check if paths exist in the sandbox filesystem.
---

{{% alert title="Prerequisite" color="info" %}}
These examples use a `SandboxTemplate` named `python-sandbox-template`. If it isn't installed in your cluster, `create_sandbox()` will return `NotFound`. See [Filesystem → Prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for a one-line install snippet.
{{% /alert %}}

## List Directory Contents

Use `sandbox.files.list()` to get the contents of a directory inside the sandbox. It returns a list of `FileEntry` objects.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# List the root directory
entries = sandbox.files.list("/")
for entry in entries:
    print(f"{entry.name:30s} {entry.type:10s} {entry.size} bytes")

sandbox.terminate()
```

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path to the directory in the sandbox |
| `timeout` | `int` | `60` | Request timeout in seconds |

**Returns:** `List[FileEntry]` — each entry has the following fields:

| Field | Type | Description |
|-------|------|-------------|
| `name` | `str` | Name of the file or directory |
| `size` | `int` | Size in bytes |
| `type` | `"file" \| "directory"` | Whether the entry is a file or directory |
| `mod_time` | `float` | POSIX timestamp of last modification |

## Check if a Path Exists

Use `sandbox.files.exists()` to check whether a file or directory exists at a given path.

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Check before reading
if sandbox.files.exists("/home/user/config.json"):
    config = sandbox.files.read("/home/user/config.json")
    print(config.decode())
else:
    print("Config file not found")

sandbox.terminate()
```

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path to check in the sandbox |
| `timeout` | `int` | `60` | Request timeout in seconds |

**Returns:** `bool` — `True` if the path exists, `False` otherwise.

## Example: Browse a Workspace

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

def print_tree(path, indent=0):
    """Recursively list sandbox directory contents."""
    entries = sandbox.files.list(path)
    for entry in entries:
        prefix = "  " * indent
        print(f"{prefix}{entry.name}/" if entry.type == "directory" else f"{prefix}{entry.name}")
        if entry.type == "directory":
            print_tree(f"{path}/{entry.name}", indent + 1)

print_tree("/home/user")

sandbox.terminate()
```
