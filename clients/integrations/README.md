# Agent Sandbox Integrations

This directory contains specialized adapters and integration packages that bridge [`k8s-agent-sandbox`](../python/agentic-sandbox-client) with popular AI agent frameworks, protocols, and evaluation harnesses.

## Available Integrations

| Integration | Distribution Name | Framework / Protocol | Description |
| :--- | :--- | :--- | :--- |
| [**`deepagents/`**](./deepagents) | `deepagents-k8s-agent-sandbox` | [LangChain DeepAgents](https://github.com/langchain-ai/deepagents) | Plugs into LangChain agent graphs as a secure, sandboxed execution backend (`K8sAgentSandbox`). |
| [**`mcp-server/`**](./mcp-server) | `k8s-agent-sandbox-mcp-server` | [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) | Exposes Sandbox provisioning and execution tools over MCP for Antigravity, Claude Desktop, Cursor, and other MCP hosts. |

## Adding a New Integration

1. Create a subdirectory under `clients/integrations/<name>/`.
2. Match the `requires-python` version floor defined in [`clients/python/agentic-sandbox-client/pyproject.toml`](../python/agentic-sandbox-client/pyproject.toml) and depend on `k8s-agent-sandbox`.
3. Include unit tests under `tests/unit/` using `pytest`.
4. Register the test suite in [`dev/tools/test-unit`](../../dev/tools/test-unit) under `PYTHON_TEST_SUITES`.
5. Exclude test packages from discovery (`[tool.setuptools.packages.find].exclude = ["tests*"]`) and configure `[tool.setuptools.exclude-package-data]` if using `setuptools_scm`.
