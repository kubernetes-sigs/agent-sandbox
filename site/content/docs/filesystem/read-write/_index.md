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
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Filesystem prerequisites]({{< ref "/docs/filesystem" >}}#prerequisites) for setup instructions.

## Write a File

Use `sandbox.files.write()` to create or overwrite a file inside the sandbox. The method accepts a path and content as either a string or bytes.


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(template="python-sandbox-template", namespace="default")

# Write a text file (string content is automatically UTF-8 encoded)
sandbox.files.write("/home/user/greeting.txt", "Hello, world!")

# Write binary content
sandbox.files.write("/home/user/data.bin", b"\x00\x01\x02\x03")

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

type FileService struct {
	sandbox *Sandbox
}

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

// WriteText writes a UTF-8 string to a file in the sandbox.
func (f *FileService) WriteText(path, content string) error {
	return f.writeRaw(path, []byte(content), "text/plain; charset=utf-8")
}

// WriteBinary writes raw bytes to a file in the sandbox.
func (f *FileService) WriteBinary(path string, data []byte) error {
	return f.writeRaw(path, data, "application/octet-stream")
}

func (f *FileService) writeRaw(path string, data []byte, contentType string) error {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (s *Sandbox) Files() *FileService {
	return &FileService{sandbox: s}
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

func main() {
	client := NewSandboxClient()
	sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	files := sandbox.Files()

	// Write a text file (string, UTF-8 encoded)
	if err := files.WriteText("/home/user/greeting.txt", "Hello, world!"); err != nil {
		panic(err)
	}

	// Write binary content
	if err := files.WriteBinary("/home/user/data.bin", []byte{0x00, 0x01, 0x02, 0x03}); err != nil {
		panic(err)
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


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
     "fmt"
     "io"
     "net/http"
     "bytes"
     "encoding/json"
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

type FileService struct {
     sandbox *Sandbox
}

func (s *Sandbox) Files() *FileService {
     return &FileService{sandbox: s}
}

func (c *SandboxClient) CreateSandbox(template, namespace string) (*Sandbox, error) {
    body, _ := json.Marshal(map[string]string{
        "template":  template,
        "namespace": namespace,
    })
    resp, err := c.http.Post(c.baseURL+"/sandboxes", "application/json", bytes.NewReader(body))
    if err != nil {
        return nil, fmt.Errorf("create sandbox: %w", err)
    }
    defer resp.Body.Close()

    var result struct {
        ID string `json:"id"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return nil, fmt.Errorf("decode sandbox response: %w", err)
    }
    return &Sandbox{ID: result.ID, client: c}, nil
}

// Read returns the raw bytes of a file in the sandbox.
// For text files, call string(data) or use ReadText below.
func (f *FileService) Read(path string) ([]byte, error) {
     url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
     resp, err := f.sandbox.client.http.Get(url)
     if err != nil {
        return nil, fmt.Errorf("read file %s: %w", path, err)
     }
     defer resp.Body.Close()
     return io.ReadAll(resp.Body)
}

// ReadText is a convenience wrapper that decodes the file bytes as UTF-8.
func (f *FileService) ReadText(path string) (string, error) {
     data, err := f.Read(path)
     if err != nil {
        return "", err
     }
     return string(data), nil
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

func main() {
     client := NewSandboxClient()
     sandbox, err := client.CreateSandbox("python-sandbox-template", "default")
     if err != nil {
        panic(err)
     }
     defer sandbox.Terminate()

     files := sandbox.Files()

     // Read a text file
     text, err := files.ReadText("/home/user/greeting.txt")
     if err != nil {
        panic(err)
     }
     fmt.Println(text) // Hello, world!

     // Read a binary file
     data, err := files.Read("/home/user/data.bin")
     if err != nil {
        panic(err)
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


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
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

func (s *Sandbox) Files() *FileService       { return &FileService{sandbox: s} }
func (s *Sandbox) Commands() *CommandService { return &CommandService{sandbox: s} }

// --- FileService ---

type FileService struct{ sandbox *Sandbox }

func (f *FileService) WriteText(path, content string) error {
	return f.writeRaw(path, []byte(content), "text/plain; charset=utf-8")
}

func (f *FileService) writeRaw(path string, data []byte, contentType string) error {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", contentType)
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
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
	return CommandResult{
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
	}, nil
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

	// Write a Python script
	script := `
import json
data = {"result": 42, "status": "ok"}
print(json.dumps(data))
`
	if err := sandbox.Files().WriteText("/home/user/run.py", script); err != nil {
		panic(err)
	}

	// Execute it
	result, err := sandbox.Commands().Run("python3 /home/user/run.py")
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Stdout)   // {"result": 42, "status": "ok"}
	fmt.Println(result.ExitCode) // 0
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}



## Async Usage

All file operations are available as async methods via `AsyncSandboxClient`:


{{< blocks/tabs name="hello-world" >}}
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
            template="python-sandbox-template", namespace="default"
        )
        await sandbox.files.write("/tmp/hello.txt", "Hello async!")
        content = await sandbox.files.read("/tmp/hello.txt")
        print(content.decode())

asyncio.run(main())
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// --- Config ---

type SandboxDirectConnectionConfig struct {
	APIURL string
}

// --- AsyncSandboxClient ---
// Go has no async/await; concurrency is via goroutines + context.
// The Python "async with" context manager maps to NewClient / defer client.Close().

type AsyncSandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewAsyncSandboxClient(cfg SandboxDirectConnectionConfig) *AsyncSandboxClient {
	return &AsyncSandboxClient{
		http:    &http.Client{},
		baseURL: cfg.APIURL,
	}
}

func (c *AsyncSandboxClient) Close() error { return nil } // hook for connection pool teardown

// --- Sandbox ---

type AsyncSandbox struct {
	ID     string
	client *AsyncSandboxClient
}

func (s *AsyncSandbox) Files() *AsyncFileService { return &AsyncFileService{sandbox: s} }

func (c *AsyncSandboxClient) CreateSandbox(ctx context.Context, template, namespace string) (*AsyncSandbox, error) {
	body := []byte(fmt.Sprintf(`{"template":%q,"namespace":%q}`, template, namespace))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sandboxes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	// parse sandbox ID from response
	var result struct{ ID string `json:"id"` }
	if err := decodeJSON(resp.Body, &result); err != nil {
		return nil, err
	}
	return &AsyncSandbox{ID: result.ID, client: c}, nil
}

func (s *AsyncSandbox) Terminate(ctx context.Context) error {
	url := fmt.Sprintf("%s/sandboxes/%s", s.client.baseURL, s.ID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// --- AsyncFileService ---

type AsyncFileService struct{ sandbox *AsyncSandbox }

func (f *AsyncFileService) Write(ctx context.Context, path, content string) error {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader([]byte(content)))
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (f *AsyncFileService) Read(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/files%s", f.sandbox.client.baseURL, f.sandbox.ID, path)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := f.sandbox.client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", path, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// --- helpers ---

func decodeJSON(r io.Reader, v any) error {
	import_bytes, _ := io.ReadAll(r)
	return json.Unmarshal(import_bytes, v)
}

// --- main ---

func main() {
	ctx := context.Background()

	cfg := SandboxDirectConnectionConfig{
		APIURL: "http://sandbox-router-svc.default.svc.cluster.local:8080",
	}

	client := NewAsyncSandboxClient(cfg)
	defer client.Close()

	sandbox, err := client.CreateSandbox(ctx, "python-sandbox-template", "default")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate(ctx)

	if err := sandbox.Files().Write(ctx, "/tmp/hello.txt", "Hello async!"); err != nil {
		panic(err)
	}

	content, err := sandbox.Files().Read(ctx, "/tmp/hello.txt")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(content)) // Hello async!
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}
