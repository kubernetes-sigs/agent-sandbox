---
title: "Read and Write Files"
linkTitle: "Read and Write Files"
weight: 1
description: >
  Read file contents from and write files to the sandbox filesystem using the Python SDK.
---

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.
- A `SandboxWarmPool` named `python-sandbox-pool` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## Write a File

Use `sandbox.files.write()` to create or overwrite a file inside the sandbox. The method accepts a path and content as either a string or bytes.


{{< blocks/tabs name="write-a-file" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

# Write a text file (string content is automatically UTF-8 encoded)
sandbox.files.write("/home/user/greeting.txt", "Hello, world!")

# Write binary content
sandbox.files.write("/home/user/data.bin", b"\x00\x01\x02\x03")

sandbox.terminate()
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()

	// WarmPoolName must be set here too to satisfy Options.validate();
	// CreateSandbox's own argument below is what actually gets used.
	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default", WarmPoolName: "python-sandbox-pool"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-pool", "default")
	if err != nil {
		log.Fatal(err)
	}

	// Write() takes a plain filename (no directory separators); the file
	// lands in the sandbox's working directory.
	if err := sb.Files().Write(ctx, "greeting.txt", []byte("Hello, world!")); err != nil {
		log.Fatal(err)
	}

	// Write binary content
	if err := sb.Files().Write(ctx, "data.bin", []byte{0x00, 0x01, 0x02, 0x03}); err != nil {
		log.Fatal(err)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path in the sandbox filesystem |
| `content` | `str \| bytes` | — | File content. Strings are UTF-8 encoded automatically |
| `timeout` | `int` | `60` | Request timeout in seconds |

## Read a File

Use `sandbox.files.read()` to download a file's contents from the sandbox. The method returns raw `bytes`.


{{< blocks/tabs name="read-a-file" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

# Read a text file
content = sandbox.files.read("/home/user/greeting.txt")
print(content.decode("utf-8"))  # 'Hello, world!'

# Read a binary file
data = sandbox.files.read("/home/user/data.bin")
print(data)  # b'\x00\x01\x02\x03'

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

	// WarmPoolName must be set here too to satisfy Options.validate();
	// CreateSandbox's own argument below is what actually gets used.
	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default", WarmPoolName: "python-sandbox-pool"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-pool", "default")
	if err != nil {
		log.Fatal(err)
	}

	// Read a text file
	text, err := sb.Files().Read(ctx, "greeting.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(text)) // Hello, world!

	// Read a binary file
	data, err := sb.Files().Read(ctx, "data.bin")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(data) // [0 1 2 3]
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}



**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `path` | `str` | — | Absolute path to the file in the sandbox |
| `timeout` | `int` | `60` | Request timeout in seconds |

**Returns:** `bytes` — the raw file content.

## Write and Execute Code

A common pattern is writing a script to the sandbox and then executing it:


{{< blocks/tabs name="write-and-execute-code" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

const runScript = `
import json
data = {"result": 42, "status": "ok"}
print(json.dumps(data))
`

func main() {
	ctx := context.Background()

	// WarmPoolName must be set here too to satisfy Options.validate();
	// CreateSandbox's own argument below is what actually gets used.
	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default", WarmPoolName: "python-sandbox-pool"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-pool", "default")
	if err != nil {
		log.Fatal(err)
	}

	// Write a Python script
	if err := sb.Files().Write(ctx, "run.py", []byte(runScript)); err != nil {
		log.Fatal(err)
	}

	// Execute it
	result, err := sb.Commands().Run(ctx, "python3 run.py")
	if err != nil {
		log.Fatal(err)
	}
	if result.ExitCode != 0 {
		log.Fatalf("run.py exited with code %d: %s", result.ExitCode, result.Stderr)
	}
	fmt.Println(result.Stdout)   // {"result": 42, "status": "ok"}
	fmt.Println(result.ExitCode) // 0
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}



## Async Usage

All file operations are available as async methods via `AsyncSandboxClient`:


{{< blocks/tabs name="async-usage" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
import asyncio
from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig

async def main():
    config = SandboxDirectConnectionConfig(
        api_url="http://sandbox-router-svc.default.svc.cluster.local:8080"
    )
    async with AsyncSandboxClient(connection_config=config) as client:
        sandbox = await client.create_sandbox(
            warmpool="python-sandbox-pool", namespace="default"
        )
        await sandbox.files.write("/tmp/hello.txt", "Hello async!")
        content = await sandbox.files.read("/tmp/hello.txt")
        print(content.decode())

asyncio.run(main())
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// Go has no async/await. Every SDK method already takes a context.Context
// for cancellation/timeouts and is safe to call concurrently from multiple
// goroutines, which is Go's equivalent of Python's async client.

func main() {
	ctx := context.Background()

	// WarmPoolName must be set here too to satisfy Options.validate();
	// CreateSandbox's own argument below is what actually gets used.
	client, err := sandbox.NewClient(ctx, sandbox.Options{Namespace: "default", WarmPoolName: "python-sandbox-pool"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DeleteAll(ctx)

	sb, err := client.CreateSandbox(ctx, "python-sandbox-pool", "default")
	if err != nil {
		log.Fatal(err)
	}

	// Write a few files concurrently.
	files := map[string]string{
		"hello.txt": "Hello async!",
		"a.txt":     "first",
		"b.txt":     "second",
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(files))
	for name, content := range files {
		wg.Add(1)
		go func(name, content string) {
			defer wg.Done()
			if err := sb.Files().Write(ctx, name, []byte(content)); err != nil {
				errs <- err
			}
		}(name, content)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		log.Fatal(err)
	}

	content, err := sb.Files().Read(ctx, "hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(content)) // Hello async!
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}
