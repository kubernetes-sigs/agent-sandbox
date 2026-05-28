# Hermes Agent in Kubernetes Sandbox Example

This project provides a complete, informational example of how to run [Hermes Agent](https://hermes-agent.nousresearch.com/) inside a Kubernetes cluster using the `Sandbox` CRD from [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox).

## Prerequisites

1.  A Kubernetes cluster (e.g., GKE, Minikube).
2.  `agent-sandbox` installed in the cluster.
3.  `kubectl` configured to access your cluster.
4.  Python 3 (for running tests and chat scripts).

## Files

-   `sandbox.yaml`: The Kubernetes manifest for the `Sandbox` resource (core infrastructure).
-   `k8s-developer.md`: The source file for the custom "Kubernetes Developer" skill.
-   `test_hermes.py`: Automated Python script to verify the agent is running.
-   `chat_hermes.py`: Interactive Python script to chat with the agent via CLI.

## How to Use

### 1. Deploy the Sandbox

#### (Optional) Add Custom Skills

If you want to load the custom "Kubernetes Developer" skill, create a ConfigMap from the file **before** deploying the sandbox:

```bash
kubectl create configmap hermes-skills --from-file=k8s-developer.md
```

Apply the core manifest to your cluster:

```bash
kubectl apply -f sandbox.yaml
```

### 2. Verify the Deployment

#### Automated Verification

Run the provided test script. It will automatically find the pod, set up port-forwarding, and query the API:

```bash
python3 test_hermes.py
```

#### Manual Verification

Check if the Sandbox resource and Pod are created:

```bash
kubectl get sandboxes
kubectl get pods
```

You should see a pod named `hermes-agent` (matching the Sandbox `metadata.name`).

### 3. Interact with the Agent

#### Option A: Interactive CLI Chat

Use the provided chat script to talk to the agent.

1.  First, find your pod name and start port-forwarding in a separate terminal:
    ```bash
    kubectl get pods
    kubectl port-forward pod/<your-pod-name> 8642:8642
    ```
2.  Then run the chat script:
    ```bash
    python3 chat_hermes.py
    ```

#### Option B: Access the Dashboard

The dashboard runs on port `9119`.

1.  Start port-forwarding:
    ```bash
    kubectl port-forward pod/<your-pod-name> 9119:9119
    ```
2.  Open `http://localhost:9119` in your browser.

## Configuration

To configure API keys or other environment variables, modify the `sandbox.yaml` file to include them in the `env` section or use a Kubernetes Secret.

Example using a Secret:

1. Create a secret with your API keys:
    ```bash
    kubectl create secret generic hermes-agent-api-keys \
      --from-literal=GEMINI_API_KEY="your_gemini_key" \
      --from-literal=OPENAI_API_KEY="your_openai_key"
    ```
2. The `sandbox.yaml` is already configured to use this secret:
    ```yaml
    env:
    - name: GEMINI_API_KEY
      valueFrom:
        secretKeyRef:
          name: hermes-agent-api-keys
          key: GEMINI_API_KEY
    ```
