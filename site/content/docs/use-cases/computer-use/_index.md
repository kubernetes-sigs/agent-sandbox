---
title: "Computer Use"
linkTitle: "Computer Use"
weight: 3
description: >
  AI agents that interact with graphical desktops, browsers, and GUI applications inside isolated sandboxes.
---

## Overview

Computer use agents interact with applications the same way a human would — viewing the screen, clicking, typing, and scrolling. These agents need access to a full graphical desktop environment or browser, which makes isolation critical: a computer use agent with unrestricted access could navigate to sensitive internal tools or exfiltrate data.

Agent Sandbox provides isolated environments with graphical desktop capabilities (VNC, browsers, IDEs) where computer use agents can operate safely.

## Why Use a Sandbox for Computer Use?

- **Visual isolation** — Each agent gets its own desktop or browser environment, completely separated from your systems.
- **Full desktop stack** — The [AIO Sandbox](/docs/use-cases/examples/aio-sandbox/) bundles VNC, VSCode, Jupyter, and Terminal in a single unified environment, based on the [agent-infra/sandbox](https://github.com/agent-infra/sandbox) project.
- **Browser task execution** — The [Gemini Computer Use](/docs/runtime-templates/computer-use/) template runs a FastAPI server that accepts browser tasks via an `/agent` endpoint and executes them using the `computer-use-preview` model with a Gemini API key.
- **Network control** — Combine with [Network Policies](/docs/use-cases/examples/network-policies/) to restrict what the agent can access on the network.

## How It Works

### Browser-based agents (Gemini Computer Use)

1. **Deploy a sandbox** with the Gemini Computer Use runtime, which runs a FastAPI server based on the [computer-use-preview](https://github.com/google-gemini/computer-use-preview) repository.
2. **Send a task** — submit a natural language query (e.g., "Go to Google and type 'Hello World' into the search bar") to the `/agent` endpoint along with a Gemini API key.
3. **Agent executes the task** — the `computer-use-preview` model controls the browser, performing clicks, typing, and navigation.
4. **Results are returned** — the endpoint returns stdout, stderr, and exit code.

### Desktop-based agents (AIO Sandbox)

1. **Deploy an AIO Sandbox** — this creates a pod with VNC, VSCode, Jupyter, and Terminal pre-installed.
2. **Access via browser** — port-forward to the sandbox and open the unified web UI at `http://localhost:8080`.
3. **Programmatic control** — use the [`agent-sandbox` Python SDK](https://github.com/agent-infra/sandbox/tree/main/sdk/python) (`pip install agent-sandbox`) to control tools inside the AIO sandbox (browser, shell, file system). Note: this SDK is for controlling tools *inside* the sandbox; for managing sandbox infrastructure, use the [`agentic-sandbox-client` SDK](/docs/python-client/).

## Examples

- [Gemini Computer Use Agent](/docs/runtime-templates/computer-use/) — A FastAPI server running the Gemini `computer-use-preview` agent. Accepts browser tasks via an `/agent` endpoint with `AgentQuery` (query + API key) and returns `AgentResponse` (stdout, stderr, exit_code). Includes Docker test scripts and Kind cluster integration.
- [All-in-One (AIO) Sandbox](/docs/use-cases/examples/aio-sandbox/) — A multi-tool sandbox with VNC, VSCode, Jupyter, and Terminal. Accessible via port-forwarding at port 8080. Controllable via the `agent-sandbox` Python SDK for programmatic browser, shell, and filesystem interaction.
