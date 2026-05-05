---
title: "Code Execution"
linkTitle: "Code Execution"
weight: 1
description: >
  Run untrusted code in fully isolated sandboxes. Ideal for code interpreters, analytics tools, and on-demand computation.
---

## Overview

Code execution is the most fundamental use case for Agent Sandbox. AI agents frequently need to generate and run code — whether to answer a data question, validate a hypothesis, or transform information. Running that code on shared infrastructure is risky: a single malicious or buggy snippet can compromise the host.

Agent Sandbox provides isolated Kubernetes pods where untrusted code runs safely. A typical pattern is a sandbox running a lightweight server (e.g., a FastAPI app with an `/execute` endpoint) that accepts commands, executes them, and returns stdout, stderr, and exit code — all within a container with its own filesystem, processes, and network stack.

## Why Use a Sandbox for Code Execution?

- **Security** — AI-generated code is unpredictable. Sandboxes prevent it from accessing your production systems, network, or data. Runtimes like [gVisor](/docs/use-cases/examples/gvisor/) or [Kata Containers](/docs/use-cases/examples/kata-containers/) provide additional kernel-level isolation.
- **Isolation** — Each sandbox runs as its own Kubernetes pod with dedicated resources and an isolated environment.
- **Stable identity and persistence** — Each sandbox has a stable hostname. Persistent storage can be attached for workloads that need to retain state across restarts.
- **Fast startup** — `SandboxWarmPool` pre-warms sandbox pods so new environments can be allocated quickly.

## How It Works

1. **Deploy a sandbox** from a runtime template — for example, the [Python Runtime Sandbox](/docs/runtime-templates/python/) deploys a FastAPI server that exposes an `/execute` endpoint accepting shell commands.
2. **Send code or commands** to the sandbox via the [Python client](/docs/python-client/) or directly through the Kubernetes API. The [ADK example](/docs/use-cases/examples/code-interpreter-agent-on-adk/) shows how an agent creates a sandbox, writes a Python file, executes it with `sandbox.commands.run()`, and reads back the output.
3. **Collect results** — the sandbox returns stdout, stderr, and exit code for each execution.
4. **Manage the lifecycle** — sandboxes can be kept running for repeated use, or terminated after a single execution (as in the ADK example which calls `sandbox.terminate()` after each run).

## Examples

- [Code Interpreter Agent on ADK](/docs/use-cases/examples/code-interpreter-agent-on-adk/) — An ADK agent (using Gemini 2.5 Flash) that wraps `SandboxClient` as a tool. For each request, it creates a sandbox from a template, writes Python code to a file, executes it via `sandbox.commands.run("python3 run.py")`, returns stdout, and terminates the sandbox.
- [Analytics Tool](/docs/guides/analytics-tool/) — A GKE-deployed analytics tool that uses LangChain with Google Generative AI to generate data analysis code (pandas, matplotlib). The generated code executes inside a sandbox pod and returns encoded chart images. Includes a JupyterLab frontend for interactive use.
- [Python Runtime Sandbox](/docs/runtime-templates/python/) — A FastAPI-based Python runtime that accepts shell commands via a `/execute` endpoint and returns stdout, stderr, and exit code. Includes a `tester.py` client script and a `run-test-kind.sh` script for automated Kind cluster setup and testing.
