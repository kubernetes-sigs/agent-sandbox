# Agent Client Protocol (ACP) Go Client Example

This example demonstrates a lightweight client in Go for the [Agent Client Protocol (ACP)](https://agentclientprotocol.com).

## Why ACP in Agent Sandbox?

The Agent Client Protocol (ACP) standardizes communication between developer environments / editors and AI coding agents via JSON-RPC 2.0 over standard I/O (`stdio`) or network streams.

In Kubernetes **Agent Sandbox**, ACP enables:
1. **Isolated Execution**: The agent (such as `gemini-cli`, `aider`, or custom agents) runs inside an isolated, stateful Sandbox container.
2. **Standardized Control**: The client interacts with the agent using a standard protocol for session lifecycle (`session/new`, `session/load`), prompting (`session/prompt`), tool permissions, and real-time updates (`session/update`).
3. **Decoupled Architecture**: Any ACP-compliant client or IDE can seamlessly connect to agent runtimes running inside Kubernetes Sandboxes over `kubectl exec`, port forwarding, or gateway routes.

## How This Example Works

The example is a minimal but complete ACP client using only Go standard library packages, split into two parts:

- **`pkg/acp`** — the protocol implementation: a `Client` with a bidirectional JSON-RPC 2.0 transport, plus Go types for the ACP messages. A single reader goroutine routes responses to pending calls by `id`, dispatches `session/update` notifications, and answers agent → client requests — all concurrently, since the agent calls back into the client while a prompt is running.
- **`main.go`** — the terminal front end: flag parsing, spawning the agent, rendering streamed updates, and answering permission and file system requests.

Together they cover the core ACP flows:

- **Handshake (`initialize`)**: Negotiates the protocol version and declares client capabilities (file system read/write).
- **Authentication (`authenticate`)**: If `session/new` is rejected because auth is required, the client authenticates with the agent's first advertised method (override with `-auth-method`).
- **Sessions (`session/new`, `session/load`)**: Creates a new session, or resumes one with `-session-id`.
- **Prompting (`session/prompt`)**: Sends user prompts and streams the agent's replies, thoughts, tool calls, and plan updates to the console as they arrive.
- **Tool call approval (`session/request_permission`)**: When the agent wants to run a tool (edit a file, run a shell command), the client shows the permission options and lets you choose; `-yolo` auto-approves.
- **File system proxy (`fs/read_text_file`, `fs/write_text_file`)**: Serves the agent's file reads/writes from the client side, as an editor would.

## Running the Example

### Interactive session against `gemini --acp`

```bash
cd examples/agentclientprotocol
go run .
```

This launches `gemini --acp` as a subprocess, creates a session in the current directory, and drops you into a prompt loop:

```text
Connected to ACP agent (protocol v1)
  Agent: gemini-cli 0.57.0
Created session dfed9cee-a170-478e-a64c-a4a42866c0d7
Type a prompt and press Enter ("exit" or Ctrl-D to quit).

> create a file named proof.txt containing hello

[permission] Agent wants to run: echo hello > proof.txt
  1) Allow for this session [allow_always]
  2) Allow [allow_once]
  3) Reject [reject_once]
Choose 1-3 (Enter = 1):
[tool] run_shell_command__call_963059 → completed
Done — proof.txt now contains "hello".
[turn ended: end_turn]
```

### One-shot prompt

```bash
cd examples/agentclientprotocol
go run . -yolo -prompt "Summarize the README in this directory"
```

`-prompt` sends a single prompt and exits when the turn completes; `-yolo` auto-approves all tool permission requests.

### Flags

| Flag | Description |
|---|---|
| `-cmd` | Agent command to spawn (default `gemini --acp`). Use shell quoting to pass the whole command as one flag value; the program then splits that value on whitespace, so individual arguments cannot themselves contain spaces (quotes inside the value are not interpreted). |
| `-cwd` | Working directory for the session (default: current directory) |
| `-prompt` | Send one prompt and exit instead of running interactively |
| `-session-id` | Resume an existing session instead of creating a new one |
| `-yolo` | Auto-approve all tool call permission requests |
| `-auth-method` | Auth method ID to use if the agent requires authentication |
| `-setup-timeout` | Timeout for the initialize/authenticate/session setup calls (default `60s`); prompt turns have no timeout |
| `-debug` | Show agent stderr, thoughts, and raw notification traffic |

### Running against an Agent inside a Sandbox

You can connect to an agent running inside a Kubernetes Sandbox pod by giving `-cmd` a command whose stdio is wired to the remote agent:

```bash
cd examples/agentclientprotocol
go run . -cmd "kubectl exec -i sandbox-claim-pod -c agent -- gemini --acp"
```
