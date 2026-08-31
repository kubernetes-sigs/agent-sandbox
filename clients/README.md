# Agent Sandbox Clients & Integrations

This directory contains client libraries, SDKs, and ecosystem adapters for interacting with the Kubernetes Agent Sandbox.

## Directory Structure

| Directory | Type | Language / Package | Description |
| :--- | :--- | :--- | :--- |
| [**`go/`**](./go) | High-Level SDK | Go (`sigs.k8s.io/agent-sandbox/clients/go/sandbox`) | Hand-written Go client wrapping `SandboxClaim` lifecycle, Gateway, port-forward, and direct connectivity. |
| [**`python/agentic-sandbox-client/`**](./python/agentic-sandbox-client) | High-Level SDK | Python (`k8s-agent-sandbox`) | Hand-written Python client for sandbox provisioning, command execution, and file management (sync and async). |
| [**`integrations/`**](./integrations) | Ecosystem Adapters | Python | Higher-level wrappers for AI agent frameworks (LangChain/DeepAgents, MCP Server). |
| [**`k8s/`**](./k8s) | **Generated** Clientset | Go (`sigs.k8s.io/agent-sandbox/clients/k8s/...`) | Auto-generated Kubernetes `client-go` typed clientset, informers, and listers for custom Go controllers. **Do not hand-edit.** |

## Choosing the Right Client

* **Building an AI agent or script (Python)**: Use [`k8s-agent-sandbox`](./python/agentic-sandbox-client) or an adapter in [`integrations/`](./integrations).
* **Building an AI agent or orchestration service (Go)**: Use the high-level Go SDK in [`go/`](./go).
* **Connecting standard LLMs via Model Context Protocol**: Use [`integrations/mcp-server/`](./integrations/mcp-server).
* **Building a custom Kubernetes controller (Go)**: Use the typed clientset/informers in [`k8s/`](./k8s).
