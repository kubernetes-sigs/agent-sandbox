---
title: "Kata Containers Isolation"
linkTitle: "Kata Containers Isolation"
weight: 11
description: >
  Harden Agent Sandbox workload isolation using Kata Containers, providing hardware virtualization with a dedicated kernel per sandbox.
---

## Overview

[Kata Containers](https://katacontainers.io/) provides hardware virtualization between the host operating system and the containerized workload. Unlike gVisor (which implements a userspace kernel), Kata Containers runs each sandbox in a lightweight virtual machine with its own dedicated kernel — there is no shared kernel between the host and the guest workload.

Agent Sandbox supports Kata Containers through the standard Kubernetes `runtimeClassName` field, the same mechanism used for gVisor. This means any sandbox workload can be hardened with Kata by setting a single field in the pod spec.

## How It Works

A kustomize overlay patches the base sandbox manifest to inject the Kata runtime class:

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: sandbox-example
spec:
  podTemplate:
    spec:
      runtimeClassName: kata-qemu
```

## Prerequisites

- Host machine that supports nested virtualization. Verify:
  ```sh
  cat /sys/module/kvm_intel/parameters/nested
  # For AMD: cat /sys/module/kvm_amd/parameters/nested
  # Output must be "Y" or 1
  ```
- [minikube](https://minikube.sigs.k8s.io/docs/start/)
- [`kubectl`](https://kubernetes.io/docs/tasks/tools/#kubectl) installed and configured to point to your cluster.
- The [Agent Sandbox Controller]({{< ref "/docs/getting_started/overview" >}}) installed on your cluster.

## Getting Started

### 1. Create a minikube cluster with Kata support

```sh
minikube start --vm-driver kvm2 --memory 8192 --container-runtime=containerd --bootstrapper=kubeadm
```

> Note: Only the `containerd` runtime is supported without additional adjustments.

### 2. Install Kata Containers

Follow the [Kata Containers Installation Guide](https://github.com/kata-containers/kata-containers/tree/main/docs/install).

### 3. Deploy a sandbox with Kata

```shell
kubectl apply -k examples/vscode-sandbox/overlays/kata
```

### 4. Verify Kata is active

```shell
kubectl wait --for=condition=Ready sandbox sandbox-example
kubectl get pods -o jsonpath=$'{range .items[*]}{.metadata.name}: {.spec.runtimeClassName}\n{end}'
```

The output should show `sandbox-example: kata-qemu`.

## Accessing the Sandbox

With Kata runtimes, direct pod port-forwarding is not compatible. Use the [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/clients/python/agentic-sandbox-client/sandbox-router) — a lightweight reverse proxy that acts as a single entry point for all sandbox traffic and routes requests to the correct sandbox pod based on an `X-Sandbox-ID` header:

```bash
# Deploy the router
kubectl apply -f clients/python/agentic-sandbox-client/sandbox_router/sandbox_router.yaml

# Port-forward to the router service
kubectl port-forward svc/sandbox-router-svc 8080:8080 -n default

# Access through the router with routing headers
curl -H "X-Sandbox-ID: sandbox-example" -H "X-Sandbox-Port: 13337" http://localhost:8080
```

For production external access (e.g., on GKE), deploy the Gateway configuration:

```bash
kubectl apply -f clients/python/agentic-sandbox-client/sandbox_router/gateway.yaml
```

## When to Use Kata Containers

Kata Containers is a good fit when you need:

- **Hardware-level isolation** — Each sandbox runs in its own VM with a dedicated kernel. There is no shared kernel between host and guest.
- **Strongest security boundary** — Ideal for multi-tenant environments or when running highly untrusted code.

For userspace isolation without hardware virtualization requirements, see [gVisor Isolation](/docs/use-cases/gvisor-isolation/).
