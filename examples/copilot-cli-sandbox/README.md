# Coding Agent with Azure OpenAI and Agent Sandbox

## Overview

This example implements an interactive coding agent that uses [Azure OpenAI](https://learn.microsoft.com/azure/ai-services/openai/) (or any OpenAI-compatible API) to generate Python code and executes it inside an Agent Sandbox pod.

The agent follows the same generate → execute → auto-fix loop as the [LangChain example](../langchain/), but uses the OpenAI Python SDK directly — no framework dependencies required.

```
┌──────────┐    task     ┌───────────────┐   code    ┌─────────────────┐
│   User   │ ──────────> │  coding_agent │ ───────── │  Azure OpenAI   │
│  (CLI)   │ <────────── │   .py         │ <──────── │  (GPT-4o)       │
└──────────┘   result    │               │           └─────────────────┘
                         │   write+run   │
                         │       │       │
                         │       ▼       │
                         │  ┌─────────┐  │
                         │  │ Sandbox │  │
                         │  │  Pod    │  │
                         │  └─────────┘  │
                         └───────────────┘
```

### How it works

1. User enters a coding task
2. Agent calls Azure OpenAI to generate Python code
3. Agent writes the code into a Sandbox pod and executes it via the [Python SDK](../../clients/python/agentic-sandbox-client/)
4. If execution fails, the error is fed back to the LLM for auto-fix (up to 3 attempts)
5. Results are printed to the user

## Prerequisites

1. **A running Kubernetes cluster** with the Agent Sandbox controller and extensions installed.
   See the [Installation Guide](../../README.md#installation).

2. **The sandbox-router deployed.** Follow the [sandbox-router setup instructions](../../clients/python/agentic-sandbox-client/sandbox-router/README.md).

3. **An Azure OpenAI resource** (or any OpenAI-compatible API key).
   See the [Azure OpenAI quickstart](https://learn.microsoft.com/azure/ai-services/openai/quickstart) for setup.

4. **Python 3.10+** and **kubectl** configured to access your cluster.

## Setup

### 1. Install the Extensions CRDs

The `SandboxTemplate` resource requires the extensions CRDs:

```shell
# Replace "vX.Y.Z" with a release tag
export VERSION="vX.Y.Z"
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```

### 2. Create the SandboxTemplate

```shell
kubectl apply -f sandbox-template.yaml
```

### 3. Install Python dependencies

```shell
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

### 4. Set your API credentials

**Azure OpenAI:**

```shell
export AZURE_OPENAI_ENDPOINT="https://<your-resource>.openai.azure.com"
export AZURE_OPENAI_API_KEY="<your-key>"
export AZURE_OPENAI_DEPLOYMENT="gpt-4o"  # your deployment name
```

**Or standard OpenAI / GitHub Models:**

```shell
export OPENAI_API_KEY="<your-key>"
export OPENAI_MODEL="gpt-4o"
# Optional: export OPENAI_BASE_URL="https://models.inference.ai.azure.com"
```

**Or GitHub Models with your GitHub token (free):**

```shell
export OPENAI_API_KEY=$(gh auth token)
export OPENAI_BASE_URL="https://models.inference.ai.azure.com"
export OPENAI_MODEL="gpt-4o"
```

## Usage

```shell
python coding_agent.py
```

Example session:

```
Connecting to sandbox (template=python-sandbox-template, ns=default)...
Sandbox ready!
============================================================
Agent Sandbox — Coding Agent (Azure OpenAI)
============================================================
Give me a coding task and I'll generate Python code,
execute it in a sandboxed environment, and show results.
Type 'exit' or 'quit' to stop.
============================================================

You: calculate the first 20 fibonacci numbers

[1/2] Generating code...
----------------------------------------
def fibonacci(n):
    fib = [0, 1]
    for i in range(2, n):
        fib.append(fib[-1] + fib[-2])
    return fib[:n]

result = fibonacci(20)
for i, num in enumerate(result):
    print(f"F({i}) = {num}")
----------------------------------------

[2/2] Executing in sandbox...

✅ Execution succeeded:
F(0) = 0
F(1) = 1
F(2) = 1
...
F(19) = 4181
```

## Configuration

| Environment Variable | Description | Default |
|---|---|---|
| `AZURE_OPENAI_ENDPOINT` | Azure OpenAI endpoint URL | — |
| `AZURE_OPENAI_API_KEY` | Azure OpenAI API key | — |
| `AZURE_OPENAI_DEPLOYMENT` | Azure OpenAI deployment name | `gpt-4o` |
| `AZURE_OPENAI_API_VERSION` | Azure OpenAI API version | `2024-12-01-preview` |
| `OPENAI_API_KEY` | OpenAI API key (used if Azure not set) | — |
| `OPENAI_MODEL` | OpenAI model name | `gpt-4o` |
| `OPENAI_BASE_URL` | Custom OpenAI-compatible endpoint | — |
| `SANDBOX_TEMPLATE` | Name of the SandboxTemplate | `python-sandbox-template` |
| `SANDBOX_NAMESPACE` | Kubernetes namespace | `default` |

## How this compares to other examples

| | This example | [LangChain](../langchain/) | [ADK](../code-interpreter-agent-on-adk/) |
|---|---|---|---|
| **LLM** | Azure OpenAI / OpenAI API | Self-hosted (codegen-350M) | Gemini |
| **Framework** | None (openai SDK only) | LangGraph | Google ADK |
| **Dependencies** | `openai`, `k8s-agent-sandbox` | transformers, torch, langgraph | google-adk |
| **GPU required** | No | Yes (or slow CPU) | No |
| **Setup time** | ~2 min | ~15 min (model download) | ~5 min |

## Cleanup

```shell
# Delete the sandbox template
kubectl delete sandboxtemplate python-sandbox-template

# Deactivate virtual environment
deactivate
```
