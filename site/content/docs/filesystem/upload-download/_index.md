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
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## Upload a Local File

Use `sandbox.files.write()` with the contents of a local file to upload it to the sandbox.


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Files() *FileService { return &FileService{sandbox: s} }

// --- FileService ---

type FileService struct{ sandbox *Sandbox }

type FileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

func (f *FileService) WriteBinary(path string, data []byte) error {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (f *FileService) List(path string) ([]FileEntry, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s?action=list", f.sandbox.client.baseURL, f.sandbox.ID, path)
	resp, err := f.sandbox.client.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", path, err)
	}
	defer resp.Body.Close()

	var entries []FileEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return entries, nil
}

// --- Sandbox lifecycle ---

func (c *SandboxClient) CreateSandbox(template, namespace string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template, "namespace": namespace})
	resp, err := c.http.Post(c.baseURL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &Sandbox{ID: result.ID, client: c}, nil
}

func (s *Sandbox) Terminate() error {
	url := fmt.Sprintf("%s/sandboxes/%s", s.client.baseURL, s.ID)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- main ---

func main() {
	client := NewSandboxClient()
	sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	// Upload a local file to the sandbox
	data, err := os.ReadFile("local_data.csv")
	if err != nil {
		panic(err)
	}
	if err := sandbox.Files().WriteBinary("/home/user/data.csv", data); err != nil {
		panic(err)
	}

	// Verify the upload
	entries, err := sandbox.Files().List("/home/user")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		fmt.Printf("%s (%d bytes)\n", entry.Name, entry.Size)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}



## Download a File from the Sandbox

Use `sandbox.files.read()` to download a file and write it locally.

{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Files()    *FileService    { return &FileService{sandbox: s} }
func (s *Sandbox) Commands() *CommandService { return &CommandService{sandbox: s} }

// --- FileService ---

type FileService struct{ sandbox *Sandbox }

func (f *FileService) Read(path string) ([]byte, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	resp, err := f.sandbox.client.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// --- CommandService ---

type CommandService struct{ sandbox *Sandbox }

func (c *CommandService) Run(cmd string) error {
	body, _ := json.Marshal(map[string]string{"command": cmd})
	url := fmt.Sprintf("%s/sandboxes/%s/commands", c.sandbox.client.baseURL, c.sandbox.ID)
	resp, err := c.sandbox.client.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("run command: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- Sandbox lifecycle ---

func (c *SandboxClient) CreateSandbox(template, namespace string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template, "namespace": namespace})
	resp, err := c.http.Post(c.baseURL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &Sandbox{ID: result.ID, client: c}, nil
}

func (s *Sandbox) Terminate() error {
	url := fmt.Sprintf("%s/sandboxes/%s", s.client.baseURL, s.ID)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- main ---

func main() {
	client := NewSandboxClient()
	sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	// Generate a file inside the sandbox
	if err := sandbox.Commands().Run("echo 'analysis complete' > /home/user/results.txt"); err != nil {
		panic(err)
	}

	// Download it to the local machine
	content, err := sandbox.Files().Read("/home/user/results.txt")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("downloaded_results.txt", content, 0644); err != nil {
		panic(err)
	}

	fmt.Printf("Downloaded %d bytes\n", len(content))
}

  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


## Upload a Directory

Upload all files from a local directory by iterating over its contents:


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Files() *FileService { return &FileService{sandbox: s} }

// --- FileService ---

type FileService struct{ sandbox *Sandbox }

type FileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

func (f *FileService) WriteBinary(path string, data []byte) error {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (f *FileService) List(path string) ([]FileEntry, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s?action=list", f.sandbox.client.baseURL, f.sandbox.ID, path)
	resp, err := f.sandbox.client.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", path, err)
	}
	defer resp.Body.Close()

	var entries []FileEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return entries, nil
}

// --- Sandbox lifecycle ---

func (c *SandboxClient) CreateSandbox(template, namespace string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template, "namespace": namespace})
	resp, err := c.http.Post(c.baseURL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &Sandbox{ID: result.ID, client: c}, nil
}

func (s *Sandbox) Terminate() error {
	url := fmt.Sprintf("%s/sandboxes/%s", s.client.baseURL, s.ID)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- main ---

func main() {
	client := NewSandboxClient()
	sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	localDir := "./my_project"
	sandboxDir := "/home/user/project"

	// Upload all files from the local directory
	entries, err := os.ReadDir(localDir)
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		localPath := filepath.Join(localDir, entry.Name())
		data, err := os.ReadFile(localPath)
		if err != nil {
			panic(err)
		}
		sandboxPath := sandboxDir + "/" + entry.Name()
		if err := sandbox.Files().WriteBinary(sandboxPath, data); err != nil {
			panic(err)
		}
		fmt.Printf("Uploaded %s\n", entry.Name())
	}

	// Verify
	uploaded, err := sandbox.Files().List(sandboxDir)
	if err != nil {
		panic(err)
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


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Files() *FileService { return &FileService{sandbox: s} }

// --- FileService ---

type FileService struct{ sandbox *Sandbox }

type FileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

func (f *FileService) List(path string) ([]FileEntry, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s?action=list", f.sandbox.client.baseURL, f.sandbox.ID, path)
	resp, err := f.sandbox.client.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", path, err)
	}
	defer resp.Body.Close()

	var entries []FileEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return entries, nil
}

func (f *FileService) Read(path string) ([]byte, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	resp, err := f.sandbox.client.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// --- Sandbox lifecycle ---

func (c *SandboxClient) CreateSandbox(template, namespace string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template, "namespace": namespace})
	resp, err := c.http.Post(c.baseURL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &Sandbox{ID: result.ID, client: c}, nil
}

func (s *Sandbox) Terminate() error {
	url := fmt.Sprintf("%s/sandboxes/%s", s.client.baseURL, s.ID)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- main ---

func main() {
	client := NewSandboxClient()
	sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	sandboxDir := "/home/user/output"
	localDir := "./downloaded_output"

	// os.makedirs(local_dir, exist_ok=True)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		panic(err)
	}

	entries, err := sandbox.Files().List(sandboxDir)
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		if entry.Type != "file" {
			continue
		}
		content, err := sandbox.Files().Read(sandboxDir + "/" + entry.Name)
		if err != nil {
			panic(err)
		}
		localPath := filepath.Join(localDir, entry.Name)
		if err := os.WriteFile(localPath, content, 0644); err != nil {
			panic(err)
		}
		fmt.Printf("Downloaded %s (%d bytes)\n", entry.Name, entry.Size)
	}
}

  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


## End-to-End Example: Upload, Process, Download

A common pattern is uploading data, processing it inside the sandbox, and downloading the results:


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Files()    *FileService    { return &FileService{sandbox: s} }
func (s *Sandbox) Commands() *CommandService { return &CommandService{sandbox: s} }

// --- FileService ---

type FileService struct{ sandbox *Sandbox }

func (f *FileService) Write(path string, data []byte) error {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (f *FileService) Read(path string) ([]byte, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	resp, err := f.sandbox.client.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// --- CommandService ---

type CommandService struct{ sandbox *Sandbox }

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (c *CommandService) Run(cmd string) (CommandResult, error) {
	body, _ := json.Marshal(map[string]string{"command": cmd})
	url := fmt.Sprintf("%s/sandboxes/%s/commands", c.sandbox.client.baseURL, c.sandbox.ID)
	resp, err := c.sandbox.client.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return CommandResult{}, fmt.Errorf("run command: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CommandResult{}, fmt.Errorf("decode response: %w", err)
	}
	return CommandResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}, nil
}

// --- Sandbox lifecycle ---

func (c *SandboxClient) CreateSandbox(template, namespace string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template, "namespace": namespace})
	resp, err := c.http.Post(c.baseURL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &Sandbox{ID: result.ID, client: c}, nil
}

func (s *Sandbox) Terminate() error {
	url := fmt.Sprintf("%s/sandboxes/%s", s.client.baseURL, s.ID)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- main ---

const processScript = `
import csv
with open('/home/user/input.csv') as f:
    reader = csv.reader(f)
    rows = list(reader)
print(f"Processed {len(rows)} rows")
with open('/home/user/output.csv', 'w') as f:
    writer = csv.writer(f)
    writer.writerows(rows)
`

func main() {
	client := NewSandboxClient()
	sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	// 1. Upload input data
	input, err := os.ReadFile("input.csv")
	if err != nil {
		panic(err)
	}
	if err := sandbox.Files().Write("/home/user/input.csv", input); err != nil {
		panic(err)
	}

	// 2. Upload the processing script
	if err := sandbox.Files().Write("/home/user/process.py", []byte(processScript)); err != nil {
		panic(err)
	}

	// 3. Run the script
	result, err := sandbox.Commands().Run("python3 /home/user/process.py")
	if err != nil {
		panic(err)
	}
	fmt.Print(result.Stdout)

	// 4. Download the output
	output, err := sandbox.Files().Read("/home/user/output.csv")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("output.csv", output, 0644); err != nil {
		panic(err)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


