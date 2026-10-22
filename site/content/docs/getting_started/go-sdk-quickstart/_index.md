---
title: "Go SDK Quickstart"
linkTitle: "Go SDK Quickstart"
weight: 1
description: >
  Create and interact with an Agent Sandbox using the Go SDK — no Kubernetes manifests or Docker builds required.
---

Agent Sandbox is a quick and easy way to start secure containers that will let agents run, execute code, call tools and interact with data. Using the SDK users can easily interact with the sandboxes without using Kubernetes primitives.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview/_index.md#installation" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/sandbox-router/README.md) deployed in your cluster when using router-based `RuntimeLegacyPython` modes (`RuntimeSandboxd` and in-cluster connectivity modes talk directly to the sandbox pod without `sandbox-router`).
- A `SandboxWarmPool` in the target namespace (for example `python-sandbox-pool` backed by `python-sandbox-template`). See the [Filesystem]({{< ref "/docs/filesystem" >}}) guide for a minimal `kubectl apply` example, or apply [python-sandbox-template.yaml](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/python-sandbox-template.yaml) and create a matching `SandboxWarmPool` whose `spec.sandboxTemplateRef.name` is `python-sandbox-template`.
- Go 1.26+ and Agent Sandbox Go client: `go get sigs.k8s.io/agent-sandbox/clients/go/sandbox`.

## Runtimes and Connection Modes

The Go SDK supports two in-sandbox runtimes via `Options.Runtime`:

- **`RuntimeLegacyPython` (default):** Speaks the `python-runtime` HTTP API on port `8888`.
- **`RuntimeSandboxd`:** Speaks the `sandboxd` hybrid API — REST filesystem, health, and metadata (`/v1/files/...`, `/v1/health`, `/v1/metadata`) on port `8080` and gRPC `ProcessService` on port `9090`.

And three pod/router `Options.Connectivity` modes (plus `GatewayName` and `APIURL` for router-based access):

- **`ConnectivityPortForward` (default):** Native SPDY port-forward tunnel (suitable for local development and CI).
- **`ConnectivityInClusterService`:** Dials the Sandbox's headless Service DNS (`Status.ServiceFQDN`, requires `spec.service: true` on the template) from inside the cluster.
- **`ConnectivityInClusterPodIP`:** Dials `Status.PodIP` directly from inside the cluster.

Learn more in the [Go Client documentation]({{< ref "/docs/go-client/_index.md" >}}).

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

	warmPoolName := "python-sandbox-pool"
	namespace := "default"

	// Create client with shared configuration (ConnectivityPortForward by default).
	// Stamp custom labels on every SandboxClaim created by this client via Options.Labels.
	client, err := sandbox.NewClient(ctx, sandbox.Options{
		WarmPoolName: warmPoolName,
		Namespace:    namespace,
		Labels:       map[string]string{"app": "quickstart"},
	})
	if err != nil {
		log.Fatal(err)
	}
	stop := client.EnableAutoCleanup()
	defer stop()
	defer client.DeleteAll(ctx)

	// Create a sandbox from the warm pool.
	sb, err := client.CreateSandbox(ctx, warmPoolName, namespace)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Sandbox ready: claim=%s sandbox=%s pod=%s\n",
		sb.ClaimName(), sb.SandboxName(), sb.PodName())

	// List SandboxClaims in the namespace filtered by label selector.
	claims, err := client.ListAllSandboxes(ctx, namespace, sandbox.WithLabelSelector("app=quickstart"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matching claims: %v\n", claims)

	// Run a command.
	result, err := sb.Run(ctx, "echo 'Hello from Go!'")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stdout: %s\n", result.Stdout)
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

## Using the `sandboxd` Runtime

When targeting a `sandboxd`-backed template or warm pool, set `Runtime: sandbox.RuntimeSandboxd` (and optionally `Connectivity: sandbox.ConnectivityInClusterService` or `sandbox.ConnectivityInClusterPodIP` when running inside the cluster). This also enables `Health()`, `Metadata()`, relative file paths, and `Delete()`.

The example below assumes the `sandboxd-warmpool` warm pool created by [a-runtime-image.yaml](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/sandboxd-sandbox/deploy/a-runtime-image.yaml) (see the [sandboxd deployment guide](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/sandboxd-sandbox/deploy/README.md) for the alternative topology):

```console
kubectl apply -f examples/sandboxd-sandbox/deploy/a-runtime-image.yaml
```

```go
client, err := sandbox.NewClient(ctx, sandbox.Options{
	Runtime:      sandbox.RuntimeSandboxd,
	Connectivity: sandbox.ConnectivityPortForward, // or ConnectivityInClusterService / ConnectivityInClusterPodIP
})
if err != nil {
	log.Fatal(err)
}
defer client.DeleteAll(ctx)

sb, err := client.CreateSandbox(ctx, "sandboxd-warmpool", "default")
if err != nil {
	log.Fatal(err)
}

health, err := sb.Health(ctx)
if err != nil {
	log.Fatal(err)
}
fmt.Printf("status=%s uptime=%ds\n", health.Status, health.UptimeSeconds)

meta, err := sb.Metadata(ctx)
if err != nil {
	log.Fatal(err)
}
fmt.Printf("metadata env=%v\n", meta.Env)
```
