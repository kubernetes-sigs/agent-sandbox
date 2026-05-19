---
title: "Snapshots"
linkTitle: "Snapshots"
weight: 15
description: >
  Create a Sandbox and optimize the GKE cluster resource usage without losing the session data in your Sandbox.
---
## Sandbox Snapshots

In many agentic workflows, you don't need a sandbox running indefinitely, but you need to preserve the exact state of a session—including filesystem changes and memory state—to resume it later.

While standard sandboxes are ephemeral, the `PodSnapshotSandboxClient` allows you to manually "freeze" a gVisor-protected sandbox and rehydrate that state into a new instance later.

## Prerequisites

This guide requires a GKE Autopilot cluster with a gVisor node pool. See [GKE Cluster Setup](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots) for infrastructure setup instructions.

- A GKE Autopilot cluster with a gVisor node pool and necessary CRDs applied.
- Google Cloud credentials configured in your environment.
- The [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxTemplate` named `simple-sandbox-template` applied to your cluster. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

### Manual Snapshot & Restore

Unlike automatic pausing, snapshots give you granular control over when state is saved. This is ideal for multi-turn agents where the environment needs to be "parked" between user prompts to save costs.

#### Basic Workflow Example

The following example demonstrates creating a sandbox, modifying its filesystem, taking a snapshot, and restoring that state into a completely new sandbox.

> Note: this example uses `simple-sandbox-template`, which you should create in your GKE cluster first. The associated resources can be found [here](https://github.com/volatilemolotov/agent-sandbox/tree/main/site/content/docs/sandbox/snapshots/source).


{{< blocks/tabs name="hello-world" >}}
  {{< blocks/tab name="Python" codelang="python" >}}
import time
from k8s_agent_sandbox.gke_extensions.snapshots import PodSnapshotSandboxClient

SLEEP_TIME = 10
def sleep():
    print(f"sleep {SLEEP_TIME} sec.")
    time.sleep(SLEEP_TIME)

# 1. Initialize the snapshot-capable client
client = PodSnapshotSandboxClient()

# 2. Create the sandbox
sandbox = client.create_sandbox("simple-sandbox-template")
print(sandbox)

# 3. Run a command that alters the filesystem (e.g., Playwright caching data)
response = sandbox.commands.run("mkdir -p /tmp/data && echo 'session_active' > /tmp/data/status.txt")
print(response)
sleep()

# 4. Snapshot the Sandbox
# This freezes the gVisor container state.
snapshot_response = sandbox.snapshots.create("my-trigger")
sleep()
assert snapshot_response is not None

print(f"Snapshot saved with ID: {snapshot_response.snapshot_uid}")

# 5. Clean up the original sandbox
sandbox.terminate()
sleep()

# 6. Later, restore the sandbox from the snapshot
restored_sandbox = client.create_sandbox("simple-sandbox-template")
is_restored = restored_sandbox.is_restored_from_snapshot(snapshot_response.snapshot_uid)
print(f"Is restored?\nAnswer: {is_restored}")

# 7. Verify the filesystem state was preserved
response = restored_sandbox.commands.run("cat /tmp/data/status.txt")
print(response.stdout) # Should output 'session_active'
  {{< /blocks/tab >}}
  {{< blocks/tab name="Go" codelang="go" >}}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const sandboxAPIBase = "http://sandbox-service.default.svc.cluster.local"

const sleepTime = 10 * time.Second

func sleep() {
	fmt.Printf("sleep %s\n", sleepTime)
	time.Sleep(sleepTime)
}

// --- PodSnapshotSandboxClient ---

type PodSnapshotSandboxClient struct {
	http    *http.Client
	baseURL string
}

func NewPodSnapshotSandboxClient() *PodSnapshotSandboxClient {
	return &PodSnapshotSandboxClient{http: &http.Client{}, baseURL: sandboxAPIBase}
}

func (c *PodSnapshotSandboxClient) CreateSandbox(template string) (*Sandbox, error) {
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
	client *PodSnapshotSandboxClient
}

func (s *Sandbox) String() string { return fmt.Sprintf("Sandbox{ID: %s}", s.ID) }

func (s *Sandbox) Commands() *CommandService  { return &CommandService{sandbox: s} }
func (s *Sandbox) Snapshots() *SnapshotService { return &SnapshotService{sandbox: s} }

func (s *Sandbox) IsRestoredFromSnapshot(snapshotUID string) (bool, error) {
	url := fmt.Sprintf("%s/sandboxes/%s/snapshot-status?uid=%s", s.client.baseURL, s.ID, snapshotUID)
	resp, err := s.client.http.Get(url)
	if err != nil {
		return false, fmt.Errorf("snapshot status: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Restored bool `json:"restored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decode response: %w", err)
	}
	return result.Restored, nil
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

func (r CommandResult) String() string {
	return fmt.Sprintf("CommandResult{ExitCode: %d, Stdout: %q}", r.ExitCode, r.Stdout)
}

// --- SnapshotService ---

type SnapshotService struct{ sandbox *Sandbox }

type SnapshotResponse struct {
	SnapshotUID string `json:"snapshot_uid"`
}

func (s *SnapshotService) Create(trigger string) (*SnapshotResponse, error) {
	body, _ := json.Marshal(map[string]string{"trigger": trigger})
	url := fmt.Sprintf("%s/sandboxes/%s/snapshots", s.sandbox.client.baseURL, s.sandbox.ID)
	resp, err := s.sandbox.client.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	defer resp.Body.Close()

	var result SnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// --- main ---

func main() {
	// 1. Initialize the snapshot-capable client
	client := NewPodSnapshotSandboxClient()

	// 2. Create the sandbox
	sandbox, err := client.CreateSandbox("simple-sandbox-template")
	if err != nil {
		panic(err)
	}
	fmt.Println(sandbox)

	// 3. Run a command that alters the filesystem
	response, err := sandbox.Commands().Run("mkdir -p /tmp/data && echo 'session_active' > /tmp/data/status.txt")
	if err != nil {
		panic(err)
	}
	fmt.Println(response)
	sleep()

	// 4. Snapshot the sandbox
	snapshotResponse, err := sandbox.Snapshots().Create("my-trigger")
	if err != nil {
		panic(err)
	}
	sleep()
	if snapshotResponse == nil {
		panic("snapshot response is nil")
	}
	fmt.Printf("Snapshot saved with ID: %s\n", snapshotResponse.SnapshotUID)

	// 5. Clean up the original sandbox
	if err := sandbox.Terminate(); err != nil {
		panic(err)
	}
	sleep()

	// 6. Restore the sandbox from the snapshot
	restoredSandbox, err := client.CreateSandbox("simple-sandbox-template")
	if err != nil {
		panic(err)
	}
	isRestored, err := restoredSandbox.IsRestoredFromSnapshot(snapshotResponse.SnapshotUID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Is restored?\nAnswer: %v\n", isRestored)

	// 7. Verify the filesystem state was preserved
	result, err := restoredSandbox.Commands().Run("cat /tmp/data/status.txt")
	if err != nil {
		panic(err)
	}
	fmt.Print(result.Stdout) // Should output 'session_active'
}
  {{< /blocks/tab >}}
{{< /blocks/tabs >}}


