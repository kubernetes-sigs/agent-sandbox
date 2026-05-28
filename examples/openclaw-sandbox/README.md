# OpenClaw Sandbox Example

This example demonstrates how to run [OpenClaw (formerly Moltbot)](https://github.com/openclaw/openclaw) inside the Agent Sandbox.

## Prerequisites

-   A Kubernetes cluster (e.g., Kind).
-   (Optional) Ensure your cluster has a RuntimeClass (e.g., `gvisor`) configured and nodes support it. This example is verified using gVisor. The `openclaw-sandbox.yaml` manifest includes a commented-out `runtimeClassName: gvisor` line. Uncomment it or update it if you are using a non-default runtime class (e.g., Kata Containers). See the [gVisor documentation](https://gvisor.dev/docs/user_guide/quick_start/kubernetes/).
-   `agent-sandbox` controller installed.

## Usage

1.  (If using Kind) Load the image into Kind:
    ```bash
    kind load docker-image ghcr.io/openclaw/openclaw:2026.3.23
    ```

2.  Generate a secure token:
    ```bash
    export OPENCLAW_GATEWAY_TOKEN="$(openssl rand -hex 32)"
    ```

3.  Apply the Sandbox resource (replacing the token placeholder):
    ```bash
    kubectl apply -f openclaw-config.yaml

    sed "s/dummy-token-for-sandbox/$OPENCLAW_GATEWAY_TOKEN/g" openclaw-sandbox.yaml | kubectl apply -f -
    ```

4.  **Access the Web UI**:

    **Option 1: Direct Port-Forward (Only if NOT using gVisor)**
    Verify the pod is running and port-forward to access it directly:
    ```bash
    kubectl port-forward pod/openclaw-sandbox 18789:18789
    ```
    Then open [http://localhost:18789](http://localhost:18789) in your browser.

    **Option 2: Access with gVisor Enabled**
    If you enable gVisor by uncommenting `runtimeClassName: gvisor` in `openclaw-sandbox.yaml`, direct `kubectl port-forward` to the pod will fail (see [Issue #158](https://github.com/kubernetes-sigs/agent-sandbox/issues/158)).

    To access the Web UI with gVisor, you must use an alternative method:
    - **Kubernetes Service**: Expose the sandbox pod via a `NodePort` or `LoadBalancer` service and access it via the service's endpoint.
    - **Router Architecture**: Use the `sandbox-router` to proxy traffic. See [agentic-sandbox-client](../../clients/python/agentic-sandbox-client) and [sandbox-router](../../clients/python/agentic-sandbox-client/sandbox-router) for instructions.

## CLI Operations

You can run OpenClaw CLI commands directly inside the sandbox container.

```bash
kubectl exec -it openclaw-sandbox -- openclaw --help
```