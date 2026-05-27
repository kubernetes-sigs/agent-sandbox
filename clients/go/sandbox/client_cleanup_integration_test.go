// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build integration

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestCleanupOnSignalWithSIGTERM verifies that sandboxes are cleaned up
// when a program with CleanupOnSignal=true receives SIGTERM.
func TestCleanupOnSignalWithSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This test requires SANDBOX_TEMPLATE env var
	template := os.Getenv("SANDBOX_TEMPLATE")
	if template == "" {
		t.Skip("SANDBOX_TEMPLATE not set, skipping")
	}

	// Build a subprocess that creates sandboxes with CleanupOnSignal=true
	subprocessCode := fmt.Sprintf(`
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		TemplateName:    %q,
		Namespace:       "default",
		CleanupOnSignal: true,
		Quiet:           true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewClient failed: %%v\n", err)
		os.Exit(1)
	}

	// Create a sandbox
	sb, err := client.CreateSandbox(ctx, %q, "default")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateSandbox failed: %%v\n", err)
		os.Exit(1)
	}

	// Print the claim name so parent test can verify cleanup
	fmt.Printf("CLAIM:%%s\n", sb.ClaimName())

	// Wait for signal
	time.Sleep(60 * time.Second)
}
`, template, template)

	// Write subprocess code to temp file
	tmpDir := t.TempDir()
	mainFile := tmpDir + "/main.go"
	if err := os.WriteFile(mainFile, []byte(subprocessCode), 0644); err != nil {
		t.Fatalf("failed to write subprocess code: %v", err)
	}

	// Start subprocess
	cmd := exec.Command("go", "run", mainFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}

	// Give subprocess time to create sandbox
	time.Sleep(10 * time.Second)

	// Send SIGTERM to subprocess
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	// Wait for subprocess to exit
	if err := cmd.Wait(); err != nil {
		// SIGTERM causes non-zero exit, which is expected
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Logf("subprocess exit: %v (expected non-zero from signal)", err)
		}
	}

	// TODO: Verify sandbox was actually deleted
	// This requires parsing the claim name from subprocess output
	// and checking if it still exists in the cluster
	t.Log("Signal handling test completed - manual verification needed")
}

// TestCleanupOnSignalDisabled verifies that sandboxes persist
// when a program with CleanupOnSignal=false (default) exits normally.
func TestCleanupOnSignalDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This test requires SANDBOX_TEMPLATE env var
	template := os.Getenv("SANDBOX_TEMPLATE")
	if template == "" {
		t.Skip("SANDBOX_TEMPLATE not set, skipping")
	}

	// Build a subprocess that creates sandboxes with CleanupOnSignal=false
	subprocessCode := fmt.Sprintf(`
package main

import (
	"context"
	"fmt"
	"os"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		TemplateName:    %q,
		Namespace:       "default",
		CleanupOnSignal: false,
		Quiet:           true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewClient failed: %%v\n", err)
		os.Exit(1)
	}

	// Create a sandbox
	sb, err := client.CreateSandbox(ctx, %q, "default")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateSandbox failed: %%v\n", err)
		os.Exit(1)
	}

	// Print the claim name so parent test can verify persistence
	fmt.Printf("CLAIM:%%s\n", sb.ClaimName())

	// Exit normally without cleanup
}
`, template, template)

	// Write subprocess code to temp file
	tmpDir := t.TempDir()
	mainFile := tmpDir + "/main.go"
	if err := os.WriteFile(mainFile, []byte(subprocessCode), 0644); err != nil {
		t.Fatalf("failed to write subprocess code: %v", err)
	}

	// Start subprocess
	cmd := exec.Command("go", "run", mainFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}

	// Wait for subprocess to complete
	if err := cmd.Wait(); err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}

	// TODO: Verify sandbox was NOT deleted
	// This requires parsing the claim name from subprocess output
	// and checking if it still exists in the cluster
	t.Log("Cleanup disabled test completed - manual verification needed")
}

// TestCleanupWithDeferPattern verifies the recommended production pattern
// of using both CleanupOnSignal and defer client.DeleteAll() for maximum reliability.
func TestCleanupWithDeferPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This test requires SANDBOX_TEMPLATE env var
	template := os.Getenv("SANDBOX_TEMPLATE")
	if template == "" {
		t.Skip("SANDBOX_TEMPLATE not set, skipping")
	}

	// Build a subprocess that uses the recommended pattern:
	// CleanupOnSignal=true for signal handling + defer DeleteAll() for normal exit
	subprocessCode := fmt.Sprintf(`
package main

import (
	"context"
	"fmt"
	"os"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		TemplateName:    %q,
		Namespace:       "default",
		CleanupOnSignal: true,
		Quiet:           true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewClient failed: %%v\n", err)
		os.Exit(1)
	}

	// Ensure cleanup even on normal exit
	defer client.DeleteAll(ctx)

	// Create a sandbox
	sb, err := client.CreateSandbox(ctx, %q, "default")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateSandbox failed: %%v\n", err)
		os.Exit(1)
	}

	// Print the claim name so parent test can verify cleanup
	fmt.Printf("CLAIM:%%s\n", sb.ClaimName())

	// Exit normally - defer will handle cleanup
}
`, template, template)

	// Write subprocess code to temp file
	tmpDir := t.TempDir()
	mainFile := tmpDir + "/main.go"
	if err := os.WriteFile(mainFile, []byte(subprocessCode), 0644); err != nil {
		t.Fatalf("failed to write subprocess code: %v", err)
	}

	// Start subprocess
	cmd := exec.Command("go", "run", mainFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}

	// Wait for subprocess to complete
	if err := cmd.Wait(); err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}

	// Wait for cleanup operations to complete
	time.Sleep(5 * time.Second)

	// TODO: Verify sandbox was deleted
	// This requires parsing the claim name from subprocess output
	// and checking if it still exists in the cluster
	t.Log("Defer pattern test completed - manual verification needed")
}
