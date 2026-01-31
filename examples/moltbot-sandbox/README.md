# Moltbot (OpenClaw) Sandbox Example

This example demonstrates how to run [OpenClaw (Moltbot)](https://github.com/openclaw/openclaw) inside the Agent Sandbox.

## Prerequisites

-   A Kubernetes cluster (e.g., Kind).
-   `agent-sandbox` controller installed.

## Usage

1.  (If using Kind) Load the image into Kind:
    ```bash
    kind load docker-image moltbot/moltbot:latest
    ```

2.  Generate a secure token:
    ```bash
    export OPENCLAW_GATEWAY_TOKEN="$(openssl rand -hex 32)"
    ```

3.  Apply the Sandbox resource (replacing the token placeholder):
    ```bash
    sed "s/dummy-token-for-sandbox/$OPENCLAW_GATEWAY_TOKEN/g" moltbot-sandbox.yaml | kubectl apply -f -
    ```

4.  Verify the pod is running and port-forward to access the Gateway:
    ```bash
    kubectl port-forward pod/<pod-name> 18789:18789
    ```

5.  **Access the Web UI**: Open [http://localhost:18789](http://localhost:18789) in your browser.

## CLI Operations

You can run Moltbot CLI commands directly inside the sandbox container.
Note: The entry point is `dist/index.mjs` in newer versions.

```bash
# Get the pod name
export POD_NAME=$(kubectl get pod -l sandbox=moltbot-sandbox -o jsonpath='{.items[0].metadata.name}')

# Check status
kubectl exec -it $POD_NAME -- node dist/index.mjs channels status

# Send a message (example)
kubectl exec -it $POD_NAME -- node dist/index.mjs message send --channel discord --to <USER_ID> "Hello from Sandbox"
```
