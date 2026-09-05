# Sandboxed Tools Example (Go)

This example demonstrates an architectural pattern for AI agents: **launching an Agent Sandbox for tool execution**, keeping the agentic loop itself outside of the sandbox, and reusing a sandbox across multiple tool calls within a session.

By reusing a sandbox and automatically extending its inactivity timeout, we avoid sandbox creation latency on subsequent tool calls, while still ensuring the sandbox is automatically cleaned up when idle.

## Architecture & Key Concepts

1. **Minimal OpenAI-Compatible Client (`pkg/llm`)**: A lightweight Go client built on `net/http` without a third-party OpenAI SDK that interacts with OpenAI-compatible API endpoints (such as the Gemini API via its OpenAI compatibility layer). It supports function calling (tools) and tool call responses.
2. **Sandbox Reuse**: The application provisions a sandbox pod on the first tool call of a session, and reuses it for subsequent tool calls. This keeps the execution overhead low.
3. **Session Persistence via Snapshots**: To maintain continuity across conversation turns and protect against inactivity cleanups or CLI restarts:
   - The application automatically snapshots the sandbox's home directory (`/home/clawtainer` by default) after tool executions at conversation boundaries.
   - These snapshots are saved as local tarball files on the host machine under `~/.local/sandboxed-tools/<session>/fs/backup-*.tar.gz`.
   - Only the last 5 backups are retained per session; older backups are automatically pruned.
   - If a session is resumed and the sandbox was cleaned up by Kubernetes (due to inactivity timeout), a new sandbox is created and the latest snapshot is automatically restored before executing the tool.

## Command-Line Arguments (CLI Options)

The application accepts the following command-line flags:

| Flag | Description | Default / Fallback |
| :--- | :--- | :--- |
| `-session` | **Required**. A unique alphanumeric name (max 40 characters) to identify this agent session and store/restore its filesystem snapshots. | None |
| `-namespace`| The Kubernetes namespace where sandbox pods are created. | `default` (overrides `SANDBOX_NAMESPACE` env var) |
| `-image` | The container image used for the temporary sandbox pod. | `debian:bookworm-slim` (overrides `SANDBOX_IMAGE` env var) |
| `-homedir` | The directory inside the sandbox that is persisted via snapshot/restore. | `/home/clawtainer` (overrides `SANDBOX_HOME_DIR` env var) |
| `-tool-timeout` | Maximum duration a single tool invocation (`run_command`, `ls`, `read`, or `write`) may run before it is cancelled. Accepts Go duration syntax, e.g. `30s`, `2m`, `1h`. `<= 0` disables the timeout. | `2m` |

> **Note:** `-tool-timeout` cancels the Kubernetes exec connection and unblocks the agent loop; it does not guarantee that every process the command started inside the container (e.g. a detached background process) has actually been terminated.

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

## Fake LLM for Testing

Setting `OPENAI_MODEL=fake-eliza` selects a built-in fake LLM instead of a real API, so the
agent plumbing can be exercised without an LLM API key or LLM endpoint access. The fake answers
every message with a question reflecting the user's words back (`"What is the capital of France?"`
=> `"What do you think it means when you say 'What is the capital of France?'?"`), and knows
one trick for testing the tool path end to end: a message of the form `run: <command>`
makes it request a `run_command` tool call and report the result. Note that the `run:` tool
path still needs Kubernetes connectivity: the command executes in an Agent Sandbox.

## Running the Example

Make sure your Kubernetes cluster is running and accessible via your active `kubeconfig` context.

```bash
# Set your API key
export GEMINI_API_KEY="your-api-key-here"

# Run the chat interface, specifying a session name
go run ./examples/sandboxed-tools/cmd/sandboxed-tools-cli -session myfirstsession
```

## Session Persistence, Sandbox Reuse & Inactivity Expiry

We aim to balance **responsiveness**, **resource efficiency**, and **cleanup guarantees**:

### 1. Sandbox Reuse (Fast Execution)
Instead of launching and deleting a sandbox on every single tool call, the application launches the sandbox Pod only on the **first tool call**. For subsequent tool calls within the same session, the application **reuses** the active sandbox directly. This cuts execution overhead down from several seconds to milliseconds, keeping the agent loop incredibly fast.

### 2. Kubernetes-Native Inactivity Expiry
To prevent orphaned containers and resource leaks in your cluster, the application leverages the Sandbox's built-in **Lifecycle Spec**:
- During creation, the sandbox is configured with a 5-minute inactivity lifetime: `Spec.Lifecycle.ShutdownTime` is set to `now + 5 minutes` and `Spec.Lifecycle.ShutdownPolicy` is set to `Delete`.
- Every time a new tool is executed, the application automatically **extends the lifecycle** by updating the sandbox's `ShutdownTime` in Kubernetes to `now + 5 minutes`.
- If no new tool calls are made for 5 minutes (e.g., because the CLI was closed, crashed, or left idle), the **Kubernetes controller automatically terminates the Pod and deletes the Sandbox resource**.

### 3. Resuming & Local Filesystem Backups
- **Message History**: Chat history is saved in real-time to a JSONL file at `~/.local/sandboxed-tools/<session-name>/sessions/latest.jsonl`, and restored automatically on startup.
- **Durable Backups**: After each set of tool executions completes (at conversation boundaries), the filesystem state of `/home/clawtainer` is archived to a local timestamped backup at `~/.local/sandboxed-tools/<session-name>/fs`. If the CLI is later restarted or the sandbox is deleted by Kubernetes due to inactivity, a new sandbox is created and restored seamlessly from the latest local backup on the next tool execution.

## Example Session

```console
================================================================================
Welcome to the Sandboxed Tools example!
Session Name: myfirstsession
Type your message (or '/exit' or '/quit' to quit):
================================================================================

User> Create a greeting file with 'Hello from Sandbox' under my home directory, then list the files there.

Agent> I have created the file `greeting.txt` inside `/home/clawtainer` containing the message 'Hello from Sandbox'.
When I listed the files inside `/home/clawtainer`, I found:
- greeting.txt

User> /exit

# (Later, resuming the same session after the sandbox was deleted)
go run ./examples/sandboxed-tools/cmd/sandboxed-tools-cli -session myfirstsession

================================================================================
Resumed session "myfirstsession" with 4 messages in history:
================================================================================
User> Create a greeting file with 'Hello from Sandbox' under my home directory, then list the files there.
Agent> I have created the file `greeting.txt` inside `/home/clawtainer` containing the message 'Hello from Sandbox'.
When I listed the files inside `/home/clawtainer`, I found:
- greeting.txt

User> Read the greeting file.

Agent> The content of the greeting file is:
Hello from Sandbox
```
