# Kubernetes Agent Sandbox MCP Server

This MCP (Model Context Protocol) server provides tools to interact with Kubernetes Agent Sandbox, allowing language models to seamlessly provision, manage, and execute operations inside isolated sandbox environments.

## Tools

The MCP server exposes the following tools:

* **`create_sandbox`**: Creates a new Kubernetes agent sandbox from a warmpool.
  * **Arguments**: `warmpool` (str), `namespace` (str), `sandbox_ready_timeout` (int, default: 180), `labels` (dict), `shutdown_after_seconds` (int), `pod_labels` (dict), `pod_annotations` (dict).
  * **Returns**: The claim name of the newly created sandbox.

* **`delete_sandbox`**: Deletes a target sandbox claim.
  * **Arguments**: `sandbox_claim_name` (str), `namespace` (str).

* **`execute_command`**: Executes a shell command inside a sandbox.
  * **Arguments**: `sandbox_claim_name` (str), `namespace` (str), `command` (str), `timeout` (int, default: 60).
  * **Returns**: Execution exit code, stdout, and stderr.

* **`upload_file`**: Uploads a file to a target sandbox.
  * **Arguments**: `sandbox_claim_name` (str), `namespace` (str), `path` (str), `content` (str), `binary` (bool, default: False), `timeout` (int, default: 60). Base64-encoded strings should be passed if `binary` is set to True.
  * **Returns**: The number of bytes written to the file.

* **`download_file`**: Downloads a file from a target sandbox.
  * **Arguments**: `sandbox_claim_name` (str), `namespace` (str), `path` (str), `binary` (bool, default: False), `timeout` (int, default: 60). If `binary` is True, it returns the contents as a base64-encoded string.
  * **Returns**: Content of the file and the number of bytes read.

*(The server also provides a `get_sandboxes` resource to fetch a list of existing sandboxes.)*

## Configuration

The server is configured using environment variables via `pydantic-settings` and their names must start with the `K8S_SANDBOX` prefix. Nested configurations are denoted by a double underscore (`__`). You can define these variables in the environment or provide them via a `.env` file in the working directory.

### Connection Configuration

You must specify the connection method to the Kubernetes backend via the `K8S_SANDBOX_CONNECTION__TYPE` variable. It accepts three possible values: `"gateway"`, `"direct"`, or `"in-cluster"`. Depending on the chosen type, different environment variables apply:

#### 1. Gateway Connection (`K8S_SANDBOX_CONNECTION__TYPE="gateway"`)
Connects via a Kubernetes Gateway API resource.
* `K8S_SANDBOX_CONNECTION__GATEWAY_NAME`: Name of the Gateway resource. (Required)
* `K8S_SANDBOX_CONNECTION__GATEWAY_NAMESPACE`: Namespace where the Gateway resource resides. (Default: `"default"`)
* `K8S_SANDBOX_CONNECTION__GATEWAY_READY_TIMEOUT`: Timeout in seconds to wait for Gateway IP. (Default: `180`)
* `K8S_SANDBOX_CONNECTION__SERVER_PORT`: Port the sandbox container listens on. (Default: `8888`)

#### 2. Direct Connection (`K8S_SANDBOX_CONNECTION__TYPE="direct"`)
Connects directly to the Sandbox router API URL.
* `K8S_SANDBOX_CONNECTION__API_URL`: Direct URL to the router. (Required)
* `K8S_SANDBOX_CONNECTION__SERVER_PORT`: Port the sandbox container listens on. (Default: `8888`)

#### 3. In-Cluster Connection (`K8S_SANDBOX_CONNECTION__TYPE="in-cluster"`)
Connects directly to the sandbox pod from within the cluster (bypassing the router) using stable K8s DNS or the Pod IP.
* `K8S_SANDBOX_CONNECTION__USE_POD_IP`: If set to `true`, connect via the Pod IP fetched from the Sandbox status instead of cluster DNS. (Default: `false`)
* `K8S_SANDBOX_CONNECTION__SERVER_PORT`: Port the sandbox container listens on. (Default: `8888`)


### General Settings

* `K8S_SANDBOX_SESSION_ID_LABEL_KEY`: The Kubernetes label key to apply for tracking session IDs on the sandboxes. (Default: `"mcp.k8s-agent-sandbox/session-id"`)
