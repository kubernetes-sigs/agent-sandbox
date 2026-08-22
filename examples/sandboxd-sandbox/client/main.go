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

// End-to-end example of the Go SDK talking to the sandboxd runtime.
//
// It exercises the full round trip against a sandboxd-backed sandbox:
//
//  1. create a sandbox from a warm pool,
//  2. write a file into /workspace over the REST filesystem,
//  3. exec a command over the gRPC ProcessService,
//  4. read the file back and print the command output.
//
// Prerequisites: a cluster with a sandboxd SandboxTemplate + SandboxWarmPool
// applied (see ../sandbox-template.yaml and the README in this directory).
// The SDK reaches sandboxd via a pod port-forward, so your kubeconfig must
// be able to port-forward to pods in the target namespace.
//
// Usage:
//
//	go run ./examples/sandboxd-sandbox/client \
//	    -warmpool sandboxd-warmpool -namespace default
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	warmPool := flag.String("warmpool", "sandboxd-warmpool", "SandboxWarmPool to claim a sandbox from.")
	namespace := flag.String("namespace", "default", "Namespace of the warm pool / sandbox.")
	path := flag.String("file", "greeting.txt", "File path (relative to /workspace) to write and read back.")
	content := flag.String("content", "hello from the sandboxd SDK example\n", "Content to write to the file.")
	flag.Parse()

	if err := run(*warmPool, *namespace, *path, *content); err != nil {
		log.Fatalf("example failed: %v", err)
	}
}

func run(warmPool, namespace, path, content string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Select the sandboxd runtime. RestConfig is left nil so the SDK loads the
	// default kubeconfig (or in-cluster config when running inside a pod).
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		Runtime: sandbox.RuntimeSandboxd,
	})
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}

	// 1. Create (claim) a sandbox. The returned handle is already connected.
	sb, err := client.CreateSandbox(ctx, warmPool, namespace)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	fmt.Printf("created sandbox %q (claim %q)\n", sb.SandboxName(), sb.ClaimName())

	// Always tear the sandbox down on exit so the example leaves nothing behind.
	defer func() {
		if err := client.DeleteSandbox(context.Background(), sb.ClaimName(), namespace); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete sandbox %q: %v\n", sb.ClaimName(), err)
		}
	}()

	// 2. Write a file into /workspace over the REST filesystem.
	if err := sb.Write(ctx, path, []byte(content)); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	fmt.Printf("wrote %d bytes to %s\n", len(content), path)

	// 3. Exec a command over the gRPC ProcessService and grab its output.
	result, err := sb.Run(ctx, "cat "+path)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	fmt.Printf("run: exit=%d\nstdout: %s", result.ExitCode, result.Stdout)
	if result.Stderr != "" {
		fmt.Printf("stderr: %s", result.Stderr)
	}

	// 4. Read the file back over REST to confirm the round trip.
	got, err := sb.Read(ctx, path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if string(got) != content {
		return fmt.Errorf("round-trip mismatch: wrote %q, read %q", content, got)
	}
	fmt.Printf("read back %d bytes, content matches\n", len(got))

	return nil
}
