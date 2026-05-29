# Sandboxed Tools Example (Go)

This example demonstrates an architectural pattern for AI agents: **launching an Agent Sandbox exclusively for tool execution**, and keeping the agentic loop itself outside of the sandbox.

By keeping the sandbox lifetime scoped strictly to the duration of a tool call, we avoid consuming resources except when we actually need them.

## Architecture & Key Concepts

1. **Minimal OpenAI-Compatible Client (`pkg/llm`)**: A lightweight Go client built on `net/http` without a third-party OpenAI SDK that interacts with OpenAI-compatible API endpoints (such as the Gemini API via its OpenAI compatibility layer). It supports function calling (tools) and tool call responses.
2. **Ephemeral Sandbox Execution**: When the LLM requests a tool call, the application provisions a temporary sandbox directly using the low-level `agentsclientset`, executes the requested tool, and immediately deletes the `Sandbox` resource once execution completes.
3. **Session Persistence via Snapshots**: To maintain continuity across different turns of a conversation, the application automatically snapshots the sandbox's home directory (`/home/clawtainer` by default) before deleting the sandbox.
   - These snapshots are saved as local tarball files on the host machine under `~/.local/sandboxed-tools/<session>/fs/backup-*.tar.gz`.
   - When a new tool call is made, the application creates a fresh sandbox and automatically restores the latest snapshot into `/home/clawtainer` before executing the tool.
   - Only the last 5 backups are retained per session; older backups are automatically pruned.

## Command-Line Arguments (CLI Options)

The application accepts the following command-line flags:

| Flag | Description | Default / Fallback |
| :--- | :--- | :--- |
| `-session` | **Required**. A unique alphanumeric name (max 40 characters) to identify this agent session and store/restore its filesystem snapshots. | None |
| `-namespace`| The Kubernetes namespace where sandbox pods are created. | `default` (overrides `SANDBOX_NAMESPACE` env var) |
| `-image` | The container image used for the temporary sandbox pod. | `debian:bookworm-slim` (overrides `SANDBOX_IMAGE` env var) |
| `-homedir` | The directory inside the sandbox that is persisted via snapshot/restore. | `/home/clawtainer` (overrides `SANDBOX_HOME_DIR` env var) |

## Configuration

The application is configured via environment variables (usually for API keys and endpoint configuration):

| Variable | Description | Default / Fallback |
| :--- | :--- | :--- |
| `GEMINI_API_KEY` | Your Gemini API key (or `OPENAI_API_KEY`). | **Required** |
| `OPENAI_BASE_URL` | The base URL for the OpenAI-compatible API. | `https://generativelanguage.googleapis.com/v1beta/openai` |
| `OPENAI_MODEL` | The model name to use for chat completions (or `MODEL`). | `gemini-3.5-flash` |
| `SANDBOX_IMAGE` | Fallback container image if `-image` flag is not set. | `debian:bookworm-slim` |
| `SANDBOX_NAMESPACE`| Fallback Kubernetes namespace if `-namespace` flag is not set. | `default` |
| `SANDBOX_HOME_DIR` | Fallback persisted directory if `-homedir` flag is not set. | `/home/clawtainer` |

## Available Tools

The LLM has access to a powerful suite of tools configured in the registry (`pkg/tools`):

* **`run_command`**: Executes an arbitrary shell command inside the sandbox container, returning `stdout`, `stderr`, and the `exit_code`.
* **`ls`**: Lists the files and directories inside a specific folder (defaults to the current directory).
* **`read`**: Reads the full contents of a file from the sandbox.
* **`write`**: Writes specified content to a file, automatically creating parent directories if they do not exist and overwriting the file if it does.

## Running the Example

Make sure your Kubernetes cluster is running and accessible via your active `kubeconfig` context.

```bash
# Set your API key
export GEMINI_API_KEY="your-api-key-here"

# Run the chat interface, specifying a session name
go run ./examples/sandboxed-tools/main.go -session myfirstsession
```

## Example Session

```
================================================================================
Welcome to the Sandboxed Tools example!
Session Name: myfirstsession (Namespace: default)
Sandbox Image: debian:bookworm-slim
Using LLM Base URL: https://generativelanguage.googleapis.com/v1beta/openai (Model: gemini-3.5-flash)
Key Concept: An Agent Sandbox is launched ONLY when a tool needs to be executed,
             and is immediately deleted afterward.
Type your message (or '/exit' or '/quit' to quit):
================================================================================

User> Create a greeting file with 'Hello from Sandbox' under my home directory, then list the files there.

I0530 12:00:00.123456   12345 main.go:744] launching sandbox for tool execution...
I0530 12:00:05.123456   12345 main.go:759] restoring filesystem to sandbox... sandbox.name="sandbox-tool-abcde"
I0530 12:00:05.124000   12345 registry.go:72] llm invoking tool tool.name="write" tool.arguments="{\"content\":\"Hello from Sandbox\",\"path\":\"/home/clawtainer/greeting.txt\"}"
I0530 12:00:05.125000   12345 write_file.go:67] creating directory in sandbox dir="/home/clawtainer"
I0530 12:00:05.500000   12345 write_file.go:75] writing file in sandbox path="/home/clawtainer/greeting.txt"
I0530 12:00:06.123456   12345 main.go:790] snapshotting filesystem from sandbox... sandbox.name="sandbox-tool-abcde"
I0530 12:00:06.200000   12345 main.go:445] saved filesystem state to new backup backup="/home/user/.local/sandboxed-tools/myfirstsession/fs/backup-20260530T120006.tar.gz"
I0530 12:00:06.201000   12345 main.go:798] deleting sandbox sandbox.name="sandbox-tool-abcde"

I0530 12:00:06.300000   12345 main.go:744] launching sandbox for tool execution...
I0530 12:00:11.300000   12345 main.go:759] restoring filesystem to sandbox... sandbox.name="sandbox-tool-fghij"
I0530 12:00:11.301000   12345 main.go:378] restoring filesystem from latest backup backup="/home/user/.local/sandboxed-tools/myfirstsession/fs/backup-20260530T120006.tar.gz"
I0530 12:00:11.305000   12345 registry.go:72] llm invoking tool tool.name="ls" tool.arguments="{\"path\":\"/home/clawtainer\"}"
I0530 12:00:11.306000   12345 list_files.go:56] listing files in sandbox path="/home/clawtainer"
I0530 12:00:12.100000   12345 main.go:790] snapshotting filesystem from sandbox... sandbox.name="sandbox-tool-fghij"
I0530 12:00:12.150000   12345 main.go:445] saved filesystem state to new backup backup="/home/user/.local/sandboxed-tools/myfirstsession/fs/backup-20260530T120012.tar.gz"
I0530 12:00:12.151000   12345 main.go:798] deleting sandbox sandbox.name="sandbox-tool-fghij"

Agent> I have created the file `greeting.txt` inside `/home/clawtainer` containing the message 'Hello from Sandbox'.
When I listed the files inside `/home/clawtainer`, I found:
- greeting.txt
```
