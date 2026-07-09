# Nullclaw Sandbox Example

This example demonstrates how to run [Nullclaw](https://github.com/nullclaw/nullclaw) — a minimal AI assistant runtime (678 KB static Zig binary) — inside the Agent Sandbox.

## Prerequisites

-   A Kubernetes cluster (e.g., Kind).
-   `agent-sandbox` controller installed.

## Usage

1.  (If using Kind) Load the image into Kind:
    ```bash
    kind load docker-image ghcr.io/nullclaw/nullclaw:v2026.5.29
    ```

2.  Apply the Sandbox resources:
    ```bash
    kubectl apply -f nullclaw-config.yaml
    kubectl apply -f nullclaw-sandbox.yaml
    ```

3.  **Access the Gateway**:

    Verify the pod is running and port-forward to access the gateway directly:
    ```bash
    kubectl port-forward pod/nullclaw-sandbox 3000:3000
    ```
    The `/health` endpoint is publicly accessible:
    ```bash
    curl http://localhost:3000/health
    ```

## Pairing

Nullclaw requires pairing before authenticated endpoints (such as `/webhook` and `/ws`) can be used. The `/health` route remains publicly accessible without pairing. Refer to the [Nullclaw documentation](https://github.com/nullclaw/nullclaw) for pairing instructions.

## CLI Operations

You can run Nullclaw CLI commands directly inside the sandbox container.

```bash
kubectl exec -it nullclaw-sandbox -- nullclaw --help
```

## Configuration

-   The `nullclaw-config.yaml` ConfigMap provides the initial `config.json` (mounted at `/nullclaw-data/config.json`). If you change the ConfigMap, restart the Sandbox/pod to pick up the updated file (it’s mounted via `subPath`). Edit it to change the default AI provider or model.
-   The `NULLCLAW_ALLOW_PUBLIC_BIND` environment variable is required for the gateway to bind to all interfaces inside the pod.
-   To configure a specific AI provider API key, add environment variables (e.g., `OPENROUTER_API_KEY`) to the Sandbox manifest.
-   Persistent data (workspace, memory) is stored on a PVC mounted at `/nullclaw-data`.
