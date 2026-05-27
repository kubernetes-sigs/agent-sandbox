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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMain handles subprocess helper process requests.
func TestMain(m *testing.M) {
	// Check if we're running as a helper process
	if os.Getenv("GO_TEST_HELPER_PROCESS") == "1" {
		runHelperProcess()
		os.Exit(0)
	}

	// Run normal tests
	os.Exit(m.Run())
}

// runHelperProcess executes the requested helper mode.
func runHelperProcess() {
	mode := os.Getenv("GO_TEST_HELPER_MODE")
	template := os.Getenv("SANDBOX_TEMPLATE")

	ctx := context.Background()

	switch mode {
	case "cleanup-on-signal":
		// Create client with CleanupOnSignal=true and wait for signal
		client, err := NewClient(ctx, Options{
			TemplateName:    template,
			Namespace:       "default",
			CleanupOnSignal: true,
			Quiet:           true,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "NewClient failed: %v\n", err)
			os.Exit(1)
		}

		sb, err := client.CreateSandbox(ctx, template, "default")
		if err != nil {
			fmt.Fprintf(os.Stderr, "CreateSandbox failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("CLAIM:%s\n", sb.ClaimName())
		time.Sleep(60 * time.Second)

	case "cleanup-disabled":
		// Create client with CleanupOnSignal=false and exit normally
		client, err := NewClient(ctx, Options{
			TemplateName:    template,
			Namespace:       "default",
			CleanupOnSignal: false,
			Quiet:           true,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "NewClient failed: %v\n", err)
			os.Exit(1)
		}

		sb, err := client.CreateSandbox(ctx, template, "default")
		if err != nil {
			fmt.Fprintf(os.Stderr, "CreateSandbox failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("CLAIM:%s\n", sb.ClaimName())

	case "defer-pattern":
		// Demonstrate recommended pattern with defer
		client, err := NewClient(ctx, Options{
			TemplateName:    template,
			Namespace:       "default",
			CleanupOnSignal: true,
			Quiet:           true,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "NewClient failed: %v\n", err)
			os.Exit(1)
		}

		defer client.DeleteAll(ctx)

		sb, err := client.CreateSandbox(ctx, template, "default")
		if err != nil {
			fmt.Fprintf(os.Stderr, "CreateSandbox failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("CLAIM:%s\n", sb.ClaimName())

	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode: %s\n", mode)
		os.Exit(1)
	}
}

// TestCleanupOnSignalWithSIGTERM verifies that sandboxes are cleaned up
// when a program with CleanupOnSignal=true receives SIGTERM.
func TestCleanupOnSignalWithSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	template := os.Getenv("SANDBOX_TEMPLATE")
	if template == "" {
		t.Skip("SANDBOX_TEMPLATE not set, skipping")
	}

	// Start subprocess using TestHelperProcess pattern
	var stdout bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=TestCleanupOnSignalWithSIGTERM")
	cmd.Env = append(os.Environ(),
		"GO_TEST_HELPER_PROCESS=1",
		"GO_TEST_HELPER_MODE=cleanup-on-signal",
		"SANDBOX_TEMPLATE="+template,
	)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}

	// Wait for sandbox to be created and claim name to be printed
	time.Sleep(10 * time.Second)

	// Send SIGTERM
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	// Wait for subprocess to exit
	_ = cmd.Wait() // Expected to exit non-zero from signal

	// Parse claim name from output
	claimName := parseClaimName(t, stdout.String())
	if claimName == "" {
		t.Fatal("failed to parse claim name from subprocess output")
	}

	// Verify sandbox was deleted
	ctx := context.Background()
	opts := Options{
		TemplateName: template,
		Quiet:        true,
	}
	opts.setDefaults()

	k8s, err := NewK8sHelper(nil)
	if err != nil {
		t.Fatalf("failed to create k8s helper: %v", err)
	}

	// Poll for claim deletion with timeout
	deleted := waitForClaimDeletion(t, ctx, k8s, claimName, "default", 30*time.Second)
	if !deleted {
		t.Errorf("claim %s was not deleted after SIGTERM", claimName)
	}
}

// TestCleanupOnSignalDisabled verifies that sandboxes persist
// when a program with CleanupOnSignal=false (default) exits normally.
func TestCleanupOnSignalDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	template := os.Getenv("SANDBOX_TEMPLATE")
	if template == "" {
		t.Skip("SANDBOX_TEMPLATE not set, skipping")
	}

	// Start subprocess
	var stdout bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=TestCleanupOnSignalDisabled")
	cmd.Env = append(os.Environ(),
		"GO_TEST_HELPER_PROCESS=1",
		"GO_TEST_HELPER_MODE=cleanup-disabled",
		"SANDBOX_TEMPLATE="+template,
	)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}

	// Parse claim name
	claimName := parseClaimName(t, stdout.String())
	if claimName == "" {
		t.Fatal("failed to parse claim name from subprocess output")
	}

	// Verify sandbox still exists
	ctx := context.Background()
	k8s, err := NewK8sHelper(nil)
	if err != nil {
		t.Fatalf("failed to create k8s helper: %v", err)
	}

	claim, err := k8s.SandboxClaimClient.SandboxClaims("default").Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("claim %s was deleted (should persist): %v", claimName, err)
	}

	t.Logf("claim %s persisted as expected", claimName)

	// Clean up manually
	client, err := NewClient(ctx, Options{
		TemplateName: template,
		Quiet:        true,
	})
	if err != nil {
		t.Fatalf("failed to create client for cleanup: %v", err)
	}

	if err := client.Delete(ctx, Key{Namespace: "default", ClaimName: claim.Name}); err != nil {
		t.Logf("warning: manual cleanup failed: %v", err)
	}
}

// TestCleanupWithDeferPattern verifies the recommended production pattern.
func TestCleanupWithDeferPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	template := os.Getenv("SANDBOX_TEMPLATE")
	if template == "" {
		t.Skip("SANDBOX_TEMPLATE not set, skipping")
	}

	// Start subprocess
	var stdout bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=TestCleanupWithDeferPattern")
	cmd.Env = append(os.Environ(),
		"GO_TEST_HELPER_PROCESS=1",
		"GO_TEST_HELPER_MODE=defer-pattern",
		"SANDBOX_TEMPLATE="+template,
	)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}

	// Parse claim name
	claimName := parseClaimName(t, stdout.String())
	if claimName == "" {
		t.Fatal("failed to parse claim name from subprocess output")
	}

	// Verify sandbox was deleted via defer
	ctx := context.Background()
	k8s, err := NewK8sHelper(nil)
	if err != nil {
		t.Fatalf("failed to create k8s helper: %v", err)
	}

	deleted := waitForClaimDeletion(t, ctx, k8s, claimName, "default", 30*time.Second)
	if !deleted {
		t.Errorf("claim %s was not deleted (defer should have cleaned up)", claimName)
	}
}

// parseClaimName extracts the claim name from subprocess output.
func parseClaimName(t *testing.T, output string) string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "CLAIM:") {
			return strings.TrimPrefix(line, "CLAIM:")
		}
	}
	return ""
}

// waitForClaimDeletion polls for claim deletion with exponential backoff.
func waitForClaimDeletion(t *testing.T, ctx context.Context, k8s *K8sHelper, name, namespace string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		_, err := k8s.SandboxClaimClient.SandboxClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// Claim is deleted (or never existed)
			t.Logf("claim %s deleted successfully", name)
			return true
		}

		// Still exists, wait and retry
		time.Sleep(2 * time.Second)
	}

	return false
}
