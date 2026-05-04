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
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## List Directory Contents

Use `sandbox.files.list()` to get the contents of a directory inside the sandbox. It returns a list of `FileEntry` objects.

{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# List the root directory
entries = sandbox.files.list("/")
for entry in entries:
    print(f"{entry.name:30s} {entry.type:10s} {entry.size} bytes")

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

// FileEntry mirrors the Python entry object with .name, .type, .size fields.
type FileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" | "directory" | "symlink"
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

	// List the root directory
	entries, err := sandbox.Files().List("/")
	if err != nil {
		panic(err)
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


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (f *FileService) Exists(path string) (bool, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequest(http.MethodHead, url, nil)
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("exists %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
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

	exists, err := sandbox.Files().Exists("/home/user/config.json")
	if err != nil {
		panic(err)
	}

	if exists {
		data, err := sandbox.Files().Read("/home/user/config.json")
		if err != nil {
			panic(err)
		}
		fmt.Println(string(data)) // mirrors content.decode()
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


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	Type string `json:"type"` // "file" | "directory" | "symlink"
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

// --- printTree ---

func printTree(sandbox *Sandbox, path string, indent int) error {
	entries, err := sandbox.Files().List(path)
	if err != nil {
		return err
	}
	prefix := strings.Repeat("  ", indent)
	for _, entry := range entries {
		if entry.Type == "directory" {
			fmt.Printf("%s%s/\n", prefix, entry.Name)
			if err := printTree(sandbox, path+"/"+entry.Name, indent+1); err != nil {
				return err
			}
		} else {
			fmt.Printf("%s%s\n", prefix, entry.Name)
		}
	}
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

	if err := printTree(sandbox, "/home/user", 0); err != nil {
		panic(err)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}

