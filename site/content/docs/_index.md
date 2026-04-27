---
title: "Documentation"
linkTitle: "Documentation"
weight: 20
menu:
  main:
    weight: 20
---

## What is Agent Sandbox?

Agent Sandbox is a Kubernetes-native platform for managing isolated, stateful, singleton workloads — purpose-built for AI agent runtimes, development environments, and any scenario that demands a long-running container with a stable identity.

At its core, Agent Sandbox introduces the `Sandbox` Custom Resource Definition (CRD) and a set of extension CRDs (`SandboxTemplate`, `SandboxClaim`, `SandboxWarmPool`) that together give you a declarative, standardized API on top of Kubernetes primitives. Instead of stitching together StatefulSets, Services, and PersistentVolumeClaims by hand, you describe the sandbox you want and let the controller handle the rest.

## Why Agent Sandbox?

### Fast sandbox provisioning

The `SandboxWarmPool` extension pre-warms a pool of pods so that when a new sandbox is needed — for a code execution request or a fresh agent session — it can be assigned in milliseconds rather than waiting for a cold pod to schedule and start. This dramatically reduces latency for interactive workloads and high-throughput agent pipelines.

### Strong, configurable isolation

Agent Sandbox is runtime-agnostic. You can pair it with [gVisor](https://gvisor.dev/) for kernel-level sandboxing or [Kata Containers](https://katacontainers.io/) for VM-grade isolation, making it suitable for executing untrusted or LLM-generated code in multi-tenant clusters. Isolation depth is a deployment choice, not a limitation of the API.

### Stable identity and persistent storage

Each Sandbox has a stable hostname and can be backed by persistent storage that survives restarts. Agents and tools can reconnect to the same environment across sessions, preserving installed packages, files, and in-progress work without any application-level coordination.

### Lifecycle management built in

The Sandbox controller handles the full lifecycle out of the box: creation, scheduled deletion, pausing (hibernation), and automatic resume on incoming network connections. Hibernation saves compute costs during idle periods while keeping state intact for seamless resumption.

### Kubernetes-native and extensible

Agent Sandbox builds on standard Kubernetes primitives and integrates cleanly with existing cluster tooling — RBAC, namespaces, network policies, and resource quotas all apply as usual. The extension CRDs let platform teams define reusable `SandboxTemplate`s so developers can claim sandboxes without needing to know the underlying configuration details.

### Client SDKs for programmatic access

Agent Sandbox provides first-class clients for both [Python](./python-client/) and [Go](./go-client/), so agents and applications can create, query, and manage sandboxes programmatically in the language that best fits their runtime and platform.

## Core capabilities

| Capability | Description |
|---|---|
| **Sandbox CRD** | Declarative API for a single, stateful pod with a stable hostname and optional persistent storage |
| **SandboxTemplate** | Reusable templates that codify runtime configuration for consistent sandbox provisioning |
| **SandboxClaim** | User-facing abstraction that provisions a sandbox from a template without exposing low-level details |
| **SandboxWarmPool** | Pre-warmed pod pools for near-instant sandbox allocation |
| **Hibernation & resume** | Pause sandboxes to free compute resources; resume automatically on network activity |
| **Runtime flexibility** | Works with standard containers, gVisor, Kata Containers, and other OCI-compatible runtimes |
| **Python SDK** | High-level client library for programmatic sandbox management in Python-based agent runtimes |
| **Go SDK** | High-level client library for programmatic sandbox management in Go services and controllers |
| **Scheduled deletion** | Automatic cleanup of sandboxes after a configurable TTL |

## Where to go next
