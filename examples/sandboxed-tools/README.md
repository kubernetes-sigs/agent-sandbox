# Sandboxed Tools Example (Go)

This example demonstrates an architectural pattern for AI agents: **launching an Agent Sandbox exclusively for tool execution**, and keeping the agentic loop itself outside of the sandbox.

By keeping the sandbox lifetime scoped strictly to the duration of a tool call, we avoid consuming resources except when we actually need them.

## Architecture & Key Concepts

1. **Minimal OpenAI-Compatible Client (`pkg/llm`)**: A lightweight Go client built on `net/http` without a third-party OpenAI SDK that interacts with OpenAI-compatible API endpoints (such as the Gemini API via its OpenAI compatibility layer). It supports function calling (tools) and tool call responses.
2. **Ephemeral Sandbox Execution**: When the LLM requests a tool call (e.g., `run_command`), the application provisions a temporary sandbox directly using the low-level `agentsclientset`, executes the requested command via the Pod "exec" API, and immediately deletes the `Sandbox` resource.

## Configuration

The application is configured via environment variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `GEMINI_API_KEY` | Your Gemini API key (or `OPENAI_API_KEY`). | **Required** |
| `OPENAI_BASE_URL` | The base URL for the OpenAI-compatible API. | `https://generativelanguage.googleapis.com/v1beta/openai` |
| `OPENAI_MODEL` | The model name to use for chat completions (or `MODEL`). | `gemini-3.5-flash` |
| `SANDBOX_IMAGE` | The container image used for the temporary sandbox pod. | `debian:bookworm-slim` |
| `SANDBOX_NAMESPACE`| The Kubernetes namespace where sandboxes are created. | `default` |

## Running the Example

```bash
# Set your API key
export GEMINI_API_KEY="your-api-key-here"

# Run the chat interface
go run ./examples/sandboxed-tools/main.go
```

## Example Session

```
================================================================================
Welcome to the Sandboxed Tools example!
Using LLM Base URL: https://generativelanguage.googleapis.com/v1beta/openai (Model: gemini-3.5-flash)
Sandbox Image: debian:bookworm-slim (Namespace: default)
Key Concept: An Agent Sandbox is launched ONLY when a tool needs to be executed,
             and is immediately deleted afterward.
Type your message (or '/exit' or '/quit' to quit):
================================================================================

User> What is the current kernel version and uptime of the sandbox?

[Tool Execution] LLM requested tool "run_command" with command: "uname -r && uptime"
I0522 12:00:00.123456   12345 main.go:448] launching sandbox for tool execution...
I0522 12:00:05.123456   12345 main.go:462] executing command in sandbox sandbox.name="sandbox-tool-abcde" command="uname -r && uptime"
I0522 12:00:06.123456   12345 main.go:465] deleting sandbox sandbox.name="sandbox-tool-abcde"
[Tool Result] stdout:
6.1.0
 12:00:05 up  1:23,  0 users,  load average: 0.00, 0.00, 0.00
stderr:

exit_code: 0

Agent> The sandbox is running kernel version 6.1.0 and has been up for 1 hour and 23 minutes.
```
