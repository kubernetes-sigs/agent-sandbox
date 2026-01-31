# Moltbot (OpenClaw) Sandbox Example

This example demonstrates how to run [OpenClaw (Moltbot)](https://github.com/openclaw/openclaw) inside the Agent Sandbox.

## Prerequisites

-   A Kubernetes cluster (e.g., Kind).
-   `agent-sandbox` controller installed.

## Usage

1.  Build the Docker image:
    ```bash
    docker build -t moltbot-sandbox:latest .
    ```

    ```

2.  (If using Kind) Load the image into Kind:
    ```bash
    kind load docker-image moltbot/moltbot:latest
    ```

3.  Generate a secure token:
    ```bash
    export CLAWDBOT_GATEWAY_TOKEN="$(openssl rand -hex 32)"
    ```

4.  Apply the Sandbox resource (replacing the token placeholder):
    ```bash
    sed "s/dummy-token-for-sandbox/$CLAWDBOT_GATEWAY_TOKEN/g" moltbot-sandbox.yaml | kubectl apply -f -
    ```

5.  Verify the pod is running and port-forward to access the Gateway:
    ```bash
    kubectl port-forward pod/<pod-name> 18789:18789
    ```
