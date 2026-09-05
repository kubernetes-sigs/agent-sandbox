---
name: "k8s-agent-sandbox-mcp"
description: "An MCP server skill for managing Kubernetes sandboxes. Enables creating, executing commands, managing files, and terminating instances via the official kubernetes-sigs/agent-sandbox MCP server."
---

# Kubernetes Agent Sandbox Manager (MCP)

> **⚠️ SECURITY WARNING:** This MCP server does not have built-in authentication. It exposes tools that can create sandboxes, execute arbitrary commands, and read or write files using the permissions of its Kubernetes service account. Before proceeding, ensure that the server is strictly isolated on a private network or secured behind an authenticating proxy.

Use this skill when a task requires running code, executing untrusted scripts, or performing heavy parallel workloads in an isolated Kubernetes environment. This skill connects to the official `kubernetes-sigs/agent-sandbox` MCP server. It can be configured using `mcp-config.json` in this skill's directory.

## Architecture & State
Unlike basic shell execution, this sandbox is **stateful**.
When you create a sandbox, a persistent Kubernetes Pod is provisioned and identified by a `sandbox_claim_name`. You can run multiple sequential commands against the same `sandbox_claim_name` (e.g., install a package, then run a script). You must retain the `sandbox_claim_name` value in context for the lifetime of the task.

## Available MCP Tools

- **`create_sandbox`**
  - **Arguments:** `warmpool` (string), `namespace` (string), `sandbox_ready_timeout` (int, optional), `labels` (dict[string, string], optional), `shutdown_after_seconds` (int, optional, defaults to 300 — the sandbox self-deletes after 5 minutes unless raised), `pod_labels` (dict[string, string], optional), `pod_annotations` (dict[string, string], optional).
  - **Returns:** JSON object with `sandbox_claim_name`.
  - **Purpose:** Create a new sandbox.

- **`execute_command`**
  - **Arguments:** `sandbox_claim_name` (string), `namespace` (string), `command` (string — shell command or python inline script), `timeout` (int — seconds before the command times out, optional).
  - **Returns:** JSON object containing `stdout`, `stderr`, and `exit_code`, or JSON with an `error` field.
  - **Purpose:** Executes commands inside the provisioned sandbox.

- **`delete_sandbox`**
  - **Arguments:** `sandbox_claim_name` (string), `namespace` (string).
  - **Returns:** Success or error JSON object.
  - **Purpose:** Destroys the Kubernetes Pod and frees resources.

- **`download_file`**
  - **Arguments:** `sandbox_claim_name` (string), `namespace` (string), `path` (string), `binary` (bool, optional), `timeout` (int, optional).
  - **Returns:** JSON object containing `content` and `bytes_read` fields.
  - **Purpose:** Download a file from a sandbox.

- **`file_exists`**
  -  **Arguments:** `sandbox_claim_name` (string), `namespace` (string), `path` (string), `timeout` (int, optional).
  -  **Returns:** JSON object containing field `exists`.
  -  **Purpose:** Check whether a file or directory exists in a sandbox.

- **`get_sandbox_status`**
  - **Arguments:** `sandbox_claim_name` (string), `namespace` (string).
  - **Returns:** JSON object containing fields `status`, `ready`, and `message`.
  - **Purpose:** Get the readiness status of a sandbox. Use this before executing commands or transferring files to confirm the sandbox is Ready (e.g. after creation, resume, or warm-pool adoption).

- **`list_files`**
  - **Arguments:** `sandbox_claim_name` (string), `namespace` (string), `path` (string), `timeout` (int, optional), `max_entries` (int, optional, default value is 1000, maximum is 10000).
  - **Returns:** JSON object containing fields `entries`, `total_entries`, and `truncated`.
  - **Purpose:** List the contents of a directory in a sandbox. At most `max_entries` entries are returned (1000 by default). When the directory holds more, the response is truncated and 'truncated' is True while 'total_entries' reports the full count.

- **`upload_file`**
  - **Arguments:** `sandbox_claim_name` (string), `namespace` (string), `path` (string), `content` (string), `binary` (bool, optional), `timeout` (int, optional).
  - **Returns:** JSON object containing field `bytes_written`.
  - **Purpose:** Upload a file to a sandbox.

## Strict Usage Workflow

ALWAYS follow this exact sequence when using the sandbox:

1. **Initialize:** Call `create_sandbox` and store the returned `sandbox_claim_name` in your context.
2. **Execute:** Call `execute_command` using the `sandbox_claim_name` as many times as needed to complete the task.
3. **Cleanup:** Call `delete_sandbox` when the task is complete, even if previous steps failed. Do not leave orphaned sandboxes running in the cluster.

### Example Workflow Concept
*(Do not write Python wrappers for this, use the provided MCP tools directly)*
1. Tool Call: `create_sandbox(warmpool="simple-sandbox-warmpool", namespace="default")` → returns `{"sandbox_claim_name": "sbx-12345"}`
2. Tool Call: `execute_command(sandbox_claim_name="sbx-12345", namespace="default", command="pip install requests")`
3. Tool Call: `execute_command(sandbox_claim_name="sbx-12345", namespace="default", command="python -c 'import requests; print(requests.get(\"https://example.com\").status_code)'")`
4. Tool Call: `delete_sandbox(sandbox_claim_name="sbx-12345", namespace="default")`
