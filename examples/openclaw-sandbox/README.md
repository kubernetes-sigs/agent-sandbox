# OpenClaw Sandbox Example

This example demonstrates how to run [OpenClaw (formerly Moltbot)](https://github.com/openclaw/openclaw) inside the Agent Sandbox.

## Prerequisites

-   A Kubernetes cluster (e.g., Kind).
-   `agent-sandbox` controller installed.

## Usage

1.  (If using Kind) Load the image into Kind:
    ```bash
    kind load docker-image ghcr.io/openclaw/openclaw:latest
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

4.  Verify the pod is running and port-forward to access the Gateway:
    ```bash
    kubectl port-forward pod/openclaw-sandbox 18789:18789
    ```

5.  **Access the Web UI**: Open [http://localhost:18789](http://localhost:18789) in your browser.

## CLI Operations

You can run OpenClaw CLI commands directly inside the sandbox container.
Note: The entry point is `dist/index.mjs` in newer versions.

```bash
# Check status
kubectl exec -it openclaw-sandbox -- node dist/index.mjs channels status

# Send a message (example)
kubectl exec -it openclaw-sandbox -- node dist/index.mjs message send --channel discord --to <USER_ID> "Hello from Sandbox"
```
