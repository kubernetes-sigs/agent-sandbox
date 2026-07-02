---
title: "List Files and Directories"
linkTitle: "List Files and Directories"
weight: 2
description: >
  List directory contents and check if paths exist in the sandbox filesystem.
---

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.
- A `SandboxWarmPool` named `python-sandbox-pool` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## List Directory Contents

Use `sandbox.files.list()` to get the contents of a directory inside the sandbox. It returns a list of `FileEntry` objects.

{{< blocks/tabs name="list-directory-contents" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

# List the root directory
entries = sandbox.files.list("/")
for entry in entries:
    print(f"{entry.name:30s} {entry.type:10s} {entry.size} bytes")

sandbox.terminate()
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()

	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-template", "default")
	if err != nil {
		log.Fatal(err)
	}

	// List the sandbox's working directory
	entries, err := sb.Files().List(ctx, ".")
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Printf("%-30s %-10s %d bytes\n", entry.Name, entry.Type, entry.Size)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


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


{{< blocks/tabs name="check-if-a-path-exists" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

# Check before reading
if sandbox.files.exists("/home/user/config.json"):
    config = sandbox.files.read("/home/user/config.json")
    print(config.decode())
else:
    print("Config file not found")

sandbox.terminate()
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()

	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-template", "default")
	if err != nil {
		log.Fatal(err)
	}

	// Check before reading
	exists, err := sb.Files().Exists(ctx, "config.json")
	if err != nil {
		log.Fatal(err)
	}
	if exists {
		data, err := sb.Files().Read(ctx, "config.json")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(data))
	} else {
		fmt.Println("Config file not found")
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path to check in the sandbox |
| `timeout` | `int` | `60` | Request timeout in seconds |

**Returns:** `bool` — `True` if the path exists, `False` otherwise.

## Example: Browse a Workspace


{{< blocks/tabs name="browse-a-workspace" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func printTree(ctx context.Context, sb *sandbox.Sandbox, path string, indent int) error {
	entries, err := sb.Files().List(ctx, path)
	if err != nil {
		return err
	}
	prefix := strings.Repeat("  ", indent)
	for _, entry := range entries {
		if entry.Type == sandbox.FileTypeDirectory {
			fmt.Printf("%s%s/\n", prefix, entry.Name)
			if err := printTree(ctx, sb, path+"/"+entry.Name, indent+1); err != nil {
				return err
			}
		} else {
			fmt.Printf("%s%s\n", prefix, entry.Name)
		}
	}
	return nil
}

func main() {
	ctx := context.Background()

	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-template", "default")
	if err != nil {
		log.Fatal(err)
	}

	if err := printTree(ctx, sb, ".", 0); err != nil {
		log.Fatal(err)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}
