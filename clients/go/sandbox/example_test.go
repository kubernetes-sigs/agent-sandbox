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

package sandbox_test

import (
	"context"
	"fmt"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// ExampleClient_cleanupOnSignal shows basic usage with automatic cleanup
// enabled. Suitable for interactive scripts and development workflows.
func ExampleClient_cleanupOnSignal() {
	ctx := context.Background()

	// Create client with automatic cleanup enabled
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		TemplateName:    "python-sandbox",
		CleanupOnSignal: true, // Enable automatic cleanup
	})
	if err != nil {
		panic(err)
	}

	// Create and use sandboxes
	sb, err := client.CreateSandbox(ctx, "python-sandbox", "default")
	if err != nil {
		panic(err)
	}

	result, err := sb.Run(ctx, "echo 'Hello from sandbox'")
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Stdout)

	// Sandboxes are automatically cleaned up on SIGINT/SIGTERM
	// Best-effort cleanup also attempted on normal program exit
}

// ExampleClient_deferPattern shows the recommended production pattern
// combining CleanupOnSignal with explicit defer for guaranteed cleanup.
func ExampleClient_deferPattern() {
	ctx := context.Background()

	client, err := sandbox.NewClient(ctx, sandbox.Options{
		TemplateName:    "python-sandbox",
		CleanupOnSignal: true, // Handle signals
	})
	if err != nil {
		panic(err)
	}

	// Recommended: Use defer for guaranteed cleanup on normal exit
	defer client.DeleteAll(ctx)

	// Create and use sandboxes
	sb, err := client.CreateSandbox(ctx, "python-sandbox", "default")
	if err != nil {
		panic(err)
	}

	result, err := sb.Run(ctx, "python --version")
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Stdout)

	// defer ensures cleanup on normal exit
	// CleanupOnSignal handles interruption
}

// ExampleNewClient_defaultBehavior shows the default behavior when
// CleanupOnSignal is not set (backward compatible).
func ExampleNewClient_defaultBehavior() {
	ctx := context.Background()

	// Default: no automatic cleanup
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		TemplateName: "python-sandbox",
		// CleanupOnSignal defaults to false
	})
	if err != nil {
		panic(err)
	}

	sb, err := client.CreateSandbox(ctx, "python-sandbox", "default")
	if err != nil {
		panic(err)
	}

	result, err := sb.Run(ctx, "echo 'Sandbox persists after exit'")
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Stdout)

	// Sandboxes are NOT automatically cleaned up
	// Manually delete if needed:
	// sb.Close(ctx)
	// or
	// client.DeleteAll(ctx)
}
