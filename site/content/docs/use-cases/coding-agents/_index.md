---
title: "Coding Agents"
linkTitle: "Coding Agents"
weight: 2
description: >
  Autonomous agents that generate, execute, and self-correct code inside secure sandboxes with persistent storage and iterative workflows.
---

## Overview

Coding agents autonomously generate code, execute it, detect errors, and self-correct — repeating until the task succeeds. Unlike one-shot code execution, these agents maintain state across multiple iterations: the sandbox keeps the agent's container, generated files, and installed dependencies alive throughout the workflow.

Agent Sandbox is well-suited for this because each sandbox is a stateful, singleton pod with a stable identity and optional persistent storage — the agent can write code, run it, inspect errors, and retry without losing state between turns.

## Why Use a Sandbox for Coding Agents?

- **Stateful across iterations** — The sandbox pod persists across multiple agent turns. Code, files, and installed packages are retained between executions.
- **Persistent storage** — Sandboxes can mount PersistentVolumeClaims so generated artifacts, cached models, and data survive pod restarts.
- **Safe execution of untrusted code** — Agents generate arbitrary code that may fail, loop, or behave unexpectedly. The sandbox isolates this from your infrastructure.
- **Automatic error correction** — Agents like the LangGraph coding agent detect execution errors and regenerate code automatically (up to a configurable retry limit).

## How It Works

1. **Deploy a sandbox** with the agent container, dependencies, and persistent storage. The [LangGraph example](/docs/use-cases/examples/langchain/) uses an init container to download the Salesforce/codegen-350M-mono model (~350M parameters) to a PVC, and a main container running the LangGraph-based agent.
2. **User provides a task** — the user attaches to the agent via `kubectl attach` and enters a coding task (e.g., "Generate code to calculate factorial of 5").
3. **Agent generates code** — using the locally-cached transformer model to produce Python code from the prompt.
4. **Code executes in the sandbox** — the agent runs the generated code and captures stdout and stderr.
5. **Error detection and self-correction** — if execution fails, the agent feeds the error back into the model and regenerates the code, repeating up to 3 iterations.
6. **Results are returned** — the final code and execution output are displayed to the user.

## Agent Examples

- [Coding Agent on LangGraph](/docs/use-cases/examples/langchain/) — A complete coding agent deployed on Kind using the Salesforce/codegen-350M-mono model with LangGraph. Architecture includes an init container for model download, a persistent volume for model caching, and a main container running Python 3.13 with PyTorch and Transformers. The agent generates Python code, executes it, and auto-fixes errors up to 3 times. Requires a HuggingFace token.
