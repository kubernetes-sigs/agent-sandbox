---
title: "Upload and Download"
linkTitle: "Upload and Download"
weight: 3
description: >
  Transfer files between your local machine and the sandbox filesystem.
---

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.
- A `SandboxWarmPool` named `python-sandbox-pool` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## Upload a Local File

Use `sandbox.files.write()` with the contents of a local file to upload it to the sandbox.


{{< blocks/tabs name="upload-a-local-file" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

# Upload a local file to the sandbox
with open("local_data.csv", "rb") as f:
    sandbox.files.write("/home/user/data.csv", f.read())

# Verify the upload
entries = sandbox.files.list("/home/user")
for entry in entries:
    print(f"{entry.name} ({entry.size} bytes)")

sandbox.terminate()
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"os"

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

	// Upload a local file to the sandbox. Write() takes a plain filename
	// (no directory separators); the file lands in the working directory.
	data, err := os.ReadFile("local_data.csv")
	if err != nil {
		log.Fatal(err)
	}
	if err := sb.Files().Write(ctx, "data.csv", data); err != nil {
		log.Fatal(err)
	}

	// Verify the upload
	entries, err := sb.Files().List(ctx, ".")
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Printf("%s (%d bytes)\n", entry.Name, entry.Size)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}



## Download a File from the Sandbox

Use `sandbox.files.read()` to download a file and write it locally.

{{< blocks/tabs name="download-a-file-from-the-sandbox" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

# Generate a file inside the sandbox
sandbox.commands.run("echo 'analysis complete' > /home/user/results.txt")

# Download it to the local machine
content = sandbox.files.read("/home/user/results.txt")
with open("downloaded_results.txt", "wb") as f:
    f.write(content)

print(f"Downloaded {len(content)} bytes")

sandbox.terminate()
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"os"

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

	// Generate a file inside the sandbox
	result, err := sb.Commands().Run(ctx, "echo 'analysis complete' > results.txt")
	if err != nil {
		log.Fatal(err)
	}
	if result.ExitCode != 0 {
		log.Fatalf("command failed with exit code %d: %s", result.ExitCode, result.Stderr)
	}

	// Download it to the local machine
	content, err := sb.Files().Read(ctx, "results.txt")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("downloaded_results.txt", content, 0644); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Downloaded %d bytes\n", len(content))
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


## Upload a Directory

Upload all files from a local directory by iterating over its contents:


{{< blocks/tabs name="upload-a-directory" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
import os
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

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

	localDir := "./my_project"

	// Write() only accepts flat filenames (no directory separators), so
	// uploaded files land together in the sandbox's working directory.
	entries, err := os.ReadDir(localDir)
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		localPath := filepath.Join(localDir, entry.Name())
		data, err := os.ReadFile(localPath)
		if err != nil {
			log.Fatal(err)
		}
		if err := sb.Files().Write(ctx, entry.Name(), data); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Uploaded %s\n", entry.Name())
	}

	// Verify
	uploaded, err := sb.Files().List(ctx, ".")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%d files in sandbox:\n", len(uploaded))
	for _, e := range uploaded {
		fmt.Printf("  %s (%d bytes)\n", e.Name, e.Size)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


## Download Multiple Files

Download all files from a sandbox directory:


{{< blocks/tabs name="download-multiple-files" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
import os
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

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

	localDir := "./downloaded_output"
	if err := os.MkdirAll(localDir, 0755); err != nil {
		log.Fatal(err)
	}

	entries, err := sb.Files().List(ctx, ".")
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Type != sandbox.FileTypeFile {
			continue
		}
		content, err := sb.Files().Read(ctx, entry.Name)
		if err != nil {
			log.Fatal(err)
		}
		localPath := filepath.Join(localDir, entry.Name)
		if err := os.WriteFile(localPath, content, 0644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Downloaded %s (%d bytes)\n", entry.Name, entry.Size)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


## End-to-End Example: Upload, Process, Download

A common pattern is uploading data, processing it inside the sandbox, and downloading the results:


{{< blocks/tabs name="end-to-end-upload-process-download" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")

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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

const processScript = `
import csv
with open('input.csv') as f:
    reader = csv.reader(f)
    rows = list(reader)
print(f"Processed {len(rows)} rows")
with open('output.csv', 'w') as f:
    writer = csv.writer(f)
    writer.writerows(rows)
`

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

	// 1. Upload input data
	input, err := os.ReadFile("input.csv")
	if err != nil {
		log.Fatal(err)
	}
	if err := sb.Files().Write(ctx, "input.csv", input); err != nil {
		log.Fatal(err)
	}

	// 2. Upload the processing script
	if err := sb.Files().Write(ctx, "process.py", []byte(processScript)); err != nil {
		log.Fatal(err)
	}

	// 3. Run the script
	result, err := sb.Commands().Run(ctx, "python3 process.py")
	if err != nil {
		log.Fatal(err)
	}
	if result.ExitCode != 0 {
		log.Fatalf("process.py exited with code %d: %s", result.ExitCode, result.Stderr)
	}
	fmt.Print(result.Stdout)

	// 4. Download the output
	output, err := sb.Files().Read(ctx, "output.csv")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("output.csv", output, 0644); err != nil {
		log.Fatal(err)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}

