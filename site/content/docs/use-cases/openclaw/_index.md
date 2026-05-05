---
title: "Agents in Sandbox — OpenClaw"
linkTitle: "Agents in Sandbox — OpenClaw"
weight: 5
description: >
  Run always-on agent environments with OpenClaw inside Agent Sandbox for persistent, long-running workloads.
---

## Overview

Some agent workloads are not short-lived tasks but persistent services that run continuously. [OpenClaw](https://github.com/openclaw/openclaw) (formerly Moltbot) is an always-on agent that runs inside Agent Sandbox, benefiting from the sandbox's stable identity, persistent storage, and Kubernetes-native lifecycle management.

This is the **always-lived** pattern: the sandbox runs indefinitely as a persistent service, responding to requests and maintaining state across restarts.

## Why Use a Sandbox for Always-On Agents?

- **Stable identity** — Each sandbox has a stable hostname and network identity, so the agent is always reachable at the same address.
- **Persistent storage** — Sandboxes can mount persistent volumes so the agent's data survives pod restarts.
- **Web UI and CLI access** — Agents like OpenClaw expose a web interface and support CLI operations, accessible via port-forwarding.
- **Token-based authentication** — Secure access to agent interfaces through gateway authentication.
- **Lifecycle management** — The agent-sandbox controller handles pod creation, restarts, and scheduled deletion without manual intervention.

## Getting Started

See the [OpenClaw Sandbox example](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/openclaw-sandbox/) for a complete walkthrough covering image loading, token generation, sandbox deployment, web UI access, and CLI operations.
