---
title: "Go SDK Quickstart"
linkTitle: "Go SDK Quickstart"
weight: 1
description: >
  Create and interact with an Agent Sandbox using the Go SDK — no Kubernetes manifests or Docker builds required.
---

Agent Sandbox is a quick and easy way to start secure containers that will let agents run, execute code, call tools and interact with data. Using the SDK users can easily interact with the sandboxes without using Kubernetes primitives.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller](/README.md/#installation) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/README.md#setup-deploying-the-router) deployed in your cluster.
- A *SandboxTemplate* created in the target namespace.
- Go 1.26+ and Agent Sandbox Go client: `go get sigs.k8s.io/agent-sandbox/clients/go/sandbox`.

## Connection Modes

In this example the client is in the *Tunnel Mode* which is suitable for local development and testing. Learn more about the other modes in the [Go Client's main page](/docs/go-client/).


## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	ctx := context.Background()

    templateName := "my-sandbox-template"

	// Create client with shared configuration.
	client, err := sandbox.NewClient(ctx, sandbox.Options{
    TemplateName: templateName,
		Namespace: "default",
	})
	if err != nil {
		log.Fatal(err)
	}
	stop := client.EnableAutoCleanup()
	defer stop()
	defer client.DeleteAll(ctx)

	// Create a sandbox.
	sb, err := client.CreateSandbox(ctx, templateName, "default")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Sandbox ready: claim=%s sandbox=%s pod=%s\n",
		sb.ClaimName(), sb.SandboxName(), sb.PodName())

	// Run a command.
	result, err := sb.Run(ctx, "echo 'Hello from Go!'")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stdout: %s", result.Stdout)
	fmt.Printf("exit_code: %d\n", result.ExitCode)

	// Write and read a file.
	if err := sb.Write(ctx, "hello.txt", []byte("Hello, world!")); err != nil {
		log.Fatal(err)
	}
	data, err := sb.Read(ctx, "hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("file content: %s\n", string(data))
}
```
