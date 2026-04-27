---
title: "gVisor Isolation"
linkTitle: "gVisor Isolation"
weight: 10
description: >
  Harden Agent Sandbox workload isolation using gVisor, a userspace kernel that intercepts application system calls to protect the host.
---

## Overview

[gVisor](https://gvisor.dev/) provides a virtualization layer between applications and the host operating system that creates a strong layer of isolation. It implements the Linux kernel in userspace and minimizes the risk of a workload gaining access to the host machine.

The `Sandbox` API provides lifecycle features useful for managing long-running sandbox workloads on Kubernetes. In real-world scenarios, you often want to combine these lifecycle features with workload isolation for running untrusted code. gVisor achieves this by intercepting application system calls and handling them in a sandboxed kernel — the container never directly touches the host kernel.

## How It Works

Agent Sandbox supports gVisor through the standard Kubernetes `runtimeClassName` field. A kustomize overlay patches the base sandbox manifest to inject `runtimeClassName: gvisor`:

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: sandbox-example
spec:
  podTemplate:
    spec:
      runtimeClassName: gvisor
```

This means any sandbox workload (VSCode, Python runtime, coding agent, etc.) can be hardened with gVisor by adding this single field to the pod spec.

## Prerequisites

- The [Agent Sandbox Controller]({{< ref "/docs/overview" >}}) installed on your cluster.
- [`kubectl`](https://kubernetes.io/docs/tasks/tools/#kubectl) installed and configured to point to your cluster.

## Getting Started

### 1. Enable gVisor on your cluster

Follow the [gVisor Kubernetes quickstart](https://gvisor.dev/docs/user_guide/quick_start/kubernetes/) to install the gVisor runtime on your cluster.

### 2. Deploy a sandbox with gVisor

Apply the kustomize overlay to create a sandbox with `runtimeClassName: gvisor`:

```shell
kubectl apply -k examples/vscode-sandbox/overlays/gvisor
```

### 3. Verify gVisor is active

```shell
kubectl wait --for=condition=Ready sandbox sandbox-example
kubectl get pods -o jsonpath=$'{range .items[*]}{.metadata.name}: {.spec.runtimeClassName}\n{end}'
```

The output should show `sandbox-example: gvisor`.

### 4. Access the sandbox

With gVisor or Kata runtimes, direct pod port-forwarding is not compatible. Use the [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/clients/python/agentic-sandbox-client/sandbox-router) instead — a lightweight reverse proxy that acts as a single entry point for all sandbox traffic and routes requests to the correct sandbox pod based on an `X-Sandbox-ID` header:

```bash
# Deploy the router
kubectl apply -f clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml

# Port-forward to the router service
kubectl port-forward svc/sandbox-router-svc 8080:8080 -n default

# Access the sandbox through the router with routing headers
curl -H "X-Sandbox-ID: sandbox-example" -H "X-Sandbox-Port: 13337" http://localhost:8080
```

## When to Use gVisor

gVisor is a good fit when you need:

- **Strong isolation without hardware virtualization** — gVisor runs in userspace and does not require nested virtualization support on the host.
- **Broad compatibility** — gVisor supports most Linux system calls, making it compatible with a wide range of container workloads.
- **Defense in depth** — Even if a container escape vulnerability exists, the attacker only reaches the gVisor userspace kernel, not the host.

For hardware-level isolation with a separate kernel per sandbox, see [Kata Containers Isolation](/docs/use-cases/kata-containers-isolation/).
