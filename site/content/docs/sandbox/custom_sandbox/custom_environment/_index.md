---
title: "Custom Environment"
linkTitle: "Custom Environment"
weight: 15
description: >
  Create a Sandbox with custom environment variables.
---
## Executing Commands with Custom Environment Variables

In many agentic workflows, you need to execute isolated commands inside the sandbox with specific environment variables (such as ephemeral API keys, testing flags, or dynamic paths) without permanently altering the global state of the container.

By extending the sandbox's FastAPI runtime, you can accept a dynamic `env` dictionary per request and merge it seamlessly into the specific execution context of that process.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster, configured to use your custom FastAPI server as its entrypoint. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

### 1. The Custom Sandbox Runtime (Server-Side)

This code runs **inside** the sandbox pod. The `ExecuteRequest` model is extended to accept an optional dictionary of environment variables. When a command is triggered, it safely clones the system's current environment variables, merges the incoming ones, and injects them into the `subprocess.run` call.

{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
import os
import shlex
import string
import subprocess
from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI()

class ExecuteRequest(BaseModel):
    command: dict[str, dict[str, str]]

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}

@app.post("/execute")
def execute_command(req: ExecuteRequest):
    try:
        current_env = os.environ.copy()

        if "env" in req.command:
            current_env.update(req.command["env"])

        raw_command = req.command["command"]["content"]
        expanded_string = string.Template(raw_command).safe_substitute(current_env)
        safe_command = shlex.split(expanded_string)

        result = subprocess.run(
            safe_command,
            capture_output=True,
            text=True,
            timeout=120,
            env=current_env,
        )

        # Return the exact schema the SDK expects
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exitCode": result.returncode
        }
    except Exception as e:
        return {
            "stdout": "",
            "stderr": str(e),
            "exitCode": 1
        }
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// --- Request / Response types (mirrors Pydantic models) ---

type CommandContent struct {
	Content string `json:"content"`
}

type ExecuteRequest struct {
	Command struct {
		Command CommandContent    `json:"command"`
		Env     map[string]string `json:"env"`
	} `json:"command"`
}

type ExecuteResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// --- Handlers ---

func healthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "Sandbox Runtime is active.",
	})
}

func executeCommand(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, ExecuteResponse{Stderr: err.Error(), ExitCode: 1})
		return
	}

	// os.environ.copy() + update with req.Command.Env
	env := envMap()
	for k, v := range req.Command.Env {
		env[k] = v
	}

	// string.Template.safe_substitute — os.Expand leaves unrecognised vars intact
	expanded := os.Expand(req.Command.Command.Content, func(key string) string {
		if v, ok := env[key]; ok {
			return v
		}
		return "$" + key // safe_substitute behaviour: leave unknown vars as-is
	})

	// shlex.split — split on whitespace, respecting quoted tokens
	args, err := shellSplit(expanded)
	if err != nil || len(args) == 0 {
		writeJSON(w, http.StatusOK, ExecuteResponse{Stderr: "invalid command: " + err.Error(), ExitCode: 1})
		return
	}

	// subprocess.run(..., timeout=120)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = envSlice(env)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := isExitError(err, &exitErr); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			stderr.WriteString(err.Error())
		}
	}

	writeJSON(w, http.StatusOK, ExecuteResponse{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	})
}

// --- shlex.split (quoted-token aware, no external dependency) ---

func shellSplit(s string) ([]string, error) {
	var args []string
	var current strings.Builder
	inQuote := rune(0)

	for _, ch := range s {
		switch {
		case inQuote != 0:
			if ch == inQuote {
				inQuote = 0
			} else {
				current.WriteRune(ch)
			}
		case ch == '\'' || ch == '"':
			inQuote = ch
		case ch == ' ' || ch == '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args, nil
}

// --- env helpers ---

func envMap() map[string]string {
	m := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func isExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// --- main ---

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", healthCheck)
	mux.HandleFunc("POST /execute", executeCommand)

	log.Println("Sandbox Runtime listening on :8000")
	if err := http.ListenAndServe(":8000", mux); err != nil {
		log.Fatal(err)
	}
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}




> Note: you can find the rest of the Sandbox Docker image [here](https://github.com/volatilemolotov/agent-sandbox/tree/main/site/content/docs/sandbox/custom_sandbox/custom_environment/source)

### 2. Client Execution Workflow

Unlike standard command executions, this client script sends a raw JSON payload directly through the SDK's run command to pass both the `command` and the `env` dictionary to the FastAPI server.

The following example demonstrates creating a sandbox, sending a command that requires a custom environment variable (`TEST=True`), and printing the modified output.


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
from k8s_agent_sandbox import SandboxClient

# 1. Initialize the client
client = SandboxClient()

# 2. Create the sandbox using your custom runtime template
sandbox = client.create_sandbox("simple-sandbox-template")

# 3. Run a command and inject environment variables via the payload
# The FastAPI server parses this into the ExecuteRequest model
payload = {
    "command": {"content": "echo $TEST"},
    "env": {"TEST": "True"}
}
response = sandbox.commands.run(payload)

# 4. Verify the output
# Because of our custom runtime logic, this will print:
# True
print(response)
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

// --- SandboxClient ---

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

func (c *SandboxClient) CreateSandbox(template string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template})
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

// --- Sandbox ---

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Commands() *CommandService { return &CommandService{sandbox: s} }

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

// --- CommandService ---

type CommandService struct{ sandbox *Sandbox }

// CommandPayload mirrors the ExecuteRequest model from the FastAPI runtime server.
// Command.Content maps to {"command": {"content": "..."}}.
// Env injects environment variables: {"env": {"KEY": "VALUE"}}.
type CommandPayload struct {
	Command struct {
		Content string `json:"content"`
	} `json:"command"`
	Env map[string]string `json:"env,omitempty"`
}

type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func (r CommandResult) String() string {
	return fmt.Sprintf("CommandResult{ExitCode: %d, Stdout: %q}", r.ExitCode, r.Stdout)
}

// Run accepts a CommandPayload, matching the Python SDK's dict-based payload.
func (c *CommandService) Run(payload CommandPayload) (CommandResult, error) {
	// The outer wrapper {"command": payload} matches the server's ExecuteRequest shape.
	body, _ := json.Marshal(map[string]any{"command": payload})
	url := fmt.Sprintf("%s/sandboxes/%s/commands", c.sandbox.client.baseURL, c.sandbox.ID)
	resp, err := c.sandbox.client.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return CommandResult{}, fmt.Errorf("run command: %w", err)
	}
	defer resp.Body.Close()

	var result CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CommandResult{}, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// --- main ---

func main() {
	// 1. Initialize the client
	client := NewSandboxClient()

	// 2. Create the sandbox
	sandbox, err := client.CreateSandbox("simple-sandbox-template")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	// 3. Run a command and inject environment variables via the payload
	var payload CommandPayload
	payload.Command.Content = "echo $TEST"
	payload.Env = map[string]string{"TEST": "True"}

	response, err := sandbox.Commands().Run(payload)
	if err != nil {
		panic(err)
	}

	// 4. Verify the output — prints: True
	fmt.Println(response.Stdout)
}package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

// --- SandboxClient ---

type SandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewSandboxClient() *SandboxClient {
	return &SandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

func (c *SandboxClient) CreateSandbox(template string) (*Sandbox, error) {
	body, _ := json.Marshal(map[string]string{"template": template})
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

// --- Sandbox ---

type Sandbox struct {
	ID     string
	client *SandboxClient
}

func (s *Sandbox) Commands() *CommandService { return &CommandService{sandbox: s} }

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

// --- CommandService ---

type CommandService struct{ sandbox *Sandbox }

// CommandPayload mirrors the ExecuteRequest model from the FastAPI runtime server.
// Command.Content maps to {"command": {"content": "..."}}.
// Env injects environment variables: {"env": {"KEY": "VALUE"}}.
type CommandPayload struct {
	Command struct {
		Content string `json:"content"`
	} `json:"command"`
	Env map[string]string `json:"env,omitempty"`
}

type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func (r CommandResult) String() string {
	return fmt.Sprintf("CommandResult{ExitCode: %d, Stdout: %q}", r.ExitCode, r.Stdout)
}

// Run accepts a CommandPayload, matching the Python SDK's dict-based payload.
func (c *CommandService) Run(payload CommandPayload) (CommandResult, error) {
	// The outer wrapper {"command": payload} matches the server's ExecuteRequest shape.
	body, _ := json.Marshal(map[string]any{"command": payload})
	url := fmt.Sprintf("%s/sandboxes/%s/commands", c.sandbox.client.baseURL, c.sandbox.ID)
	resp, err := c.sandbox.client.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return CommandResult{}, fmt.Errorf("run command: %w", err)
	}
	defer resp.Body.Close()

	var result CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CommandResult{}, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// --- main ---

func main() {
	// 1. Initialize the client
	client := NewSandboxClient()

	// 2. Create the sandbox
	sandbox, err := client.CreateSandbox("simple-sandbox-template")
	if err != nil {
		panic(err)
	}
	defer sandbox.Terminate()

	// 3. Run a command and inject environment variables via the payload
	var payload CommandPayload
	payload.Command.Content = "echo $TEST"
	payload.Env = map[string]string{"TEST": "True"}

	response, err := sandbox.Commands().Run(payload)
	if err != nil {
		panic(err)
	}

	// 4. Verify the output — prints: True
	fmt.Println(response.Stdout)
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}

