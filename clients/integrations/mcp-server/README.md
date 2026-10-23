# Kubernetes Agent Sandbox MCP Server

This MCP (Model Context Protocol) server provides tools to interact with Kubernetes Agent Sandbox, allowing language models to seamlessly provision, manage, and execute operations inside isolated sandbox environments.

## Tools

The MCP server exposes the following tools:

* **`create_sandbox`**: Creates a new Kubernetes agent sandbox from a warmpool.
  * **Arguments**:
    * `warmpool` (str)
    * `namespace` (str)
    * `sandbox_ready_timeout` (int, default: 180)
    * `labels` (dict)
    * `shutdown_after_seconds` (int, default: 300). Set as `None` to disable a timeout.
    * `pod_labels` (dict)
    * `pod_annotations` (dict)
  * **Returns**: The claim name of the newly created sandbox.

* **`delete_sandbox`**: Deletes a target sandbox claim.
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)

* **`execute_command`**: Executes a shell command inside a sandbox.
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)
    * `command` (str)
    * `timeout` (int, default: 60)
    * `max_output_chars` (int, default: 30000, min: 128, max: 300000) — Combined stdout/stderr character budget, including omission markers.
  * **Returns**: `exit_code`, `stdout`, and `stderr`, plus `stdout_total_chars`, `stderr_total_chars`, `stdout_truncated`, and `stderr_truncated`.
  * **Output budget**: stdout and stderr initially share the budget equally (stderr gets the odd character), and unused capacity is available to the other stream. Short output is unchanged. Each oversized stream keeps equal-sized beginning and end excerpts, with at most one extra character at the end, separated by `[... N characters omitted ...]`. The marker and its surrounding newlines count toward the budget; the minimum leaves room for a marker in both streams.
    Character counts use Unicode code points, not UTF-8 bytes, visual characters, or model tokens. The `*_total_chars` fields describe the SDK's output before this tool truncates it; the `*_truncated` fields indicate whether this tool omitted content. Truncation preserves the original exit code, including nonzero exits, and never reruns the command.
  * **Compatibility**: Output exceeding the default budget is now truncated, and the response includes four new metadata fields. Callers needing a larger excerpt can set `max_output_chars` up to 300000; clients with strict response schemas must accept the new fields. No output files are created. This limit applies to stdout/stderr text in completed results, not exception messages or the complete serialized MCP response. The SDK has already received the full result, so this does not reduce runtime capture memory.

* **`upload_file`**: Uploads a file to a target sandbox.
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)
    * `path` (str)
    * `content` (str)
    * `binary` (bool, default: False) — Base64-encoded strings should be passed if `binary` is set to True.
    * `timeout` (int, default: 60)
  * **Returns**: The number of bytes written to the file.

* **`download_file`**: Downloads a file from a target sandbox.
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)
    * `path` (str)
    * `binary` (bool, default: False) — If True, returns the contents as a base64-encoded string.
    * `timeout` (int, default: 60)
  * **Returns**: Content of the file and the number of bytes read.

* **`list_files`**: Lists the contents of a directory in a sandbox.
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)
    * `path` (str)
    * `timeout` (int, default: 60)
    * `max_entries` (int, default: 1000, max: 10000) — Caps how many entries are returned.
  * **Returns**: The directory entries (each with `name`, `size`, `type` (`file` or `directory`) and `mod_time` as a POSIX timestamp), plus `total_entries` — the full count before truncation — and `truncated`.
  * **Note**: the response is capped at `max_entries` so that listing a directory with very many
    entries cannot exhaust an LLM's context window. When `truncated` is `True`, narrow the path or use
    `execute_command` for a targeted listing. The cap bounds the MCP response only; the SDK's
    `files.list()` has already materialized the full listing before this tool sees it.

* **`file_exists`**: Checks whether a file or directory exists in a sandbox.
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)
    * `path` (str)
    * `timeout` (int, default: 60)
  * **Returns**: Whether the path exists. A missing path is a successful `exists: false` result, not an error.

* **`get_sandbox_status`**: Reports the readiness of a sandbox, derived from its Kubernetes conditions. 
  * **Arguments**:
    * `sandbox_claim_name` (str)
    * `namespace` (str)
  * **Returns**: `status` (one of `SandboxReady`, `SandboxNotReady`, or `SandboxNotFound`), `ready` (True only when the `Ready` condition is True), and `message` (the Kubernetes condition message, empty if none is set).

*(The server also provides a `get_sandboxes` resource to fetch a list of existing sandboxes.)*

## Configuration

The server is configured using environment variables via `pydantic-settings` and their names must start with the `K8S_SANDBOX` prefix. Nested configurations are denoted by a double underscore (`__`). You can define these variables in the environment or provide them via a `.env` file in the working directory.

### Connection Configuration

You must specify the connection method to the Kubernetes backend via the `K8S_SANDBOX_CONNECTION__TYPE` variable. It accepts four possible values: `"gateway"`, `"direct"`, `"in-cluster"`, or `"sandboxd-in-cluster"`. Depending on the chosen type, different environment variables apply:

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

#### 4. sandboxd In-Cluster Connection (`K8S_SANDBOX_CONNECTION__TYPE="sandboxd-in-cluster"`)
Connects directly to [sandboxd](../../../packages/sandboxd/USER_GUIDE.md) in the sandbox pod from within the cluster. It needs no sandbox-router and no `kubectl port-forward`. Commands run over sandboxd's gRPC port and file tools use its REST port.
* `K8S_SANDBOX_CONNECTION__MODE`: `in-cluster-service` (uses `Sandbox.status.serviceFQDN`, so the template needs `spec.service: true`) or `in-cluster-pod-ip` (uses `Sandbox.status.podIPs`). Neither falls back to the other. (Required)
* `K8S_SANDBOX_CONNECTION__REST_PORT`: sandboxd REST port. (Default: `8080`)
* `K8S_SANDBOX_CONNECTION__GRPC_PORT`: sandboxd gRPC port. (Default: `9090`)

The warm pool's template must run sandboxd, as in [examples/sandboxd-sandbox](../../../examples/sandboxd-sandbox/sandbox-template.yaml). The default `Managed` network policy admits only sandbox-router, so add a `spec.networkPolicy.ingress` rule to the template that admits the MCP server on TCP ports `8080` and `9090`. Supplying `spec.networkPolicy` replaces all secure defaults, so also add the `spec.networkPolicy.egress` rules the sandbox needs: an omitted egress list denies all outbound traffic. The server's service account needs `create`, `get`, `list`, `watch` and `delete` on `sandboxclaims`, and `get`, `list` and `watch` on `sandboxes`. It needs no `pods/portforward`. See the [sandboxd guide](../../../packages/sandboxd/USER_GUIDE.md#agent-sandbox-sdk-access) for details.


### General Settings

* `K8S_SANDBOX_SESSION_ID_LABEL_KEY`: The Kubernetes label key to apply for tracking session IDs on the sandboxes. (Default: `"mcp.k8s-agent-sandbox/session-id"`)
* `K8S_SANDBOX_PROBE_NAMESPACE`: Namespace the `/readyz` probe lists SandboxClaims in to confirm the Kubernetes API is reachable. Must be a namespace the server's service account can list claims in. (Default: `"default"`)

## Health checks

The server exposes two endpoints for Kubernetes probes:

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | **Liveness.** The process is up and routing HTTP. Never contacts the Kubernetes API. |
| `GET /readyz` | **Readiness.** Lists SandboxClaims in `K8S_SANDBOX_PROBE_NAMESPACE` to confirm the Kubernetes API is reachable. Returns `200` with `{"ready": true}`, or `503` with `{"ready": false, "reason": "..."}`. |

`/readyz` failure reasons are `"starting"` (the server has not finished initializing) and
`"kubernetes API unavailable"`. The reason is deliberately generic — the endpoint is
unauthenticated, so the underlying exception is logged rather than returned.

`/healthz` is deliberately independent of the Kubernetes API: wiring a liveness probe to an
external dependency turns a transient control-plane blip into a rolling restart of every replica.
Use `/readyz` — which does check — to pull a pod out of the Service instead.

Because `/readyz` makes a real API call, keep `periodSeconds` modest rather than polling it
aggressively:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8000
  initialDelaySeconds: 5
readinessProbe:
  httpGet:
    path: /readyz
    port: 8000
  initialDelaySeconds: 5
  periodSeconds: 15
```

## Security

The server ships with **no authentication**: the Dockerfile serves streamable HTTP on `0.0.0.0:8000`, and any client that can reach that port gets a fresh session and full access to every tool (create sandboxes, execute arbitrary commands, read/write files) across any namespace its Kubernetes service account can touch.

Do not expose this server directly to untrusted networks. Deploy it behind an authenticating proxy or wire up [fastmcp's auth support](https://gofastmcp.com/servers/auth/authentication/) directly.
