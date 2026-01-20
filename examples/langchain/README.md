# Coding Agent on Agent-Sandbox and LangGraph

## Architecture

### Components

1. **Agent-Sandbox Controller**: Kubernetes operator that manages sandboxed workloads
2. **Init Container**: Downloads and caches the ML model on first run
3. **Main Agent Container**: Runs the LangGraph-based coding agent with local model inference
4. **Persistent Volume Claim**: Stores the cached model

### Technology Stack

- **Kubernetes**: Kind (Kubernetes in Docker)
- **ML Framework**: Transformers, PyTorch
- **Agent Framework**: LangGraph
- **Model**: Salesforce/codegen-350M-mono (350M parameters)
- **Language**: Python 3.13

## Prerequisites

### Required Software

```bash
# Verify installations
go version              # Go 1.19+
make --version          # GNU Make
python3 --version       # Python 3.13+
docker --version        # Docker 20.10+
kind --version          # Kind 0.17+
kubectl version --client # kubectl 1.25+
```

### Installation Links

- [Go](https://golang.org/doc/install)
- [Make](https://www.gnu.org/software/make/)
- [Python 3](https://www.python.org/downloads/)
- [Docker](https://docs.docker.com/get-docker/)
- [Docker buildx](https://github.com/docker/buildx)
- [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/)

## Setup Instructions

### 1. Deploy Agent-Sandbox to Kind

You can find agent-sandbox setup instructions [here](../../README.md#installation).

```bash
# Verify installation
kubectl -n agent-sandbox-system wait --for=condition=Ready pod -l app=agent-sandbox-controller
```

The `kubectl wait` command will exit when the pod is ready.

### 2. Set Up HuggingFace Token

```bash
# Export your HuggingFace token
export HF_TOKEN='your_huggingface_token_here'
```

Get a token from [HuggingFace](https://huggingface.co/settings/tokens).

### 3. Clone the repository and navigate to the examples, coding-agent directory

```bash
# Clone the repository
git clone https://github.com/kubernetes-sigs/agent-sandbox.git

# Navigate to the examples/coding-agent directory
cd examples/coding-agent
```

### 4. Set your huggingface token

Update `<HF_TOKEN>` placeholder in `deployment.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: coding-agent-hf-token
  namespace: default
type: Opaque
stringData:
  token: "<HF_TOKEN>"  # Replace with your actual HuggingFace token
```
### 5. Build and Deploy
```bash
# Build init container image
docker build -f Dockerfile.init -t model-downloader:latest .

# Build agent image
docker build -t coding-agent:latest .

# Load images into Kind
kind load docker-image model-downloader:latest --name agent-sandbox
kind load docker-image coding-agent:latest --name agent-sandbox

# Deploy to Kubernetes
kubectl apply -f deployment.yaml

# Watch init container download model (first run only, takes 3-5 minutes)
kubectl logs -f coding-agent-sandbox -c model-downloader

# Wait for pod to be ready
kubectl wait --for=condition=ready pod -l app=coding-agent --timeout=600s
```

### 6. Use the Agent

```bash
# Attach to the agent & hit enter to get interraction prompt
kubectl attach -it coding-agent-sandbox

# Example usage:
# You: Generate code to calculate factorial of 5
# [Agent generates, executes, and potentially fixes code automatically]
```

## Workflow

### Agent Execution Flow

1. **User Input**: User provides a coding task
2. **Code Generation**: LLM generates Python code based on the task
3. **Execution**: Code is executed in a sandboxed environment
4. **Error Detection**: If execution fails, error is captured
5. **Auto-Fix**: LLM attempts to fix the code (up to 3 iterations)
6. **Result**: Final code and execution output are displayed

## Troubleshooting

### Out of Memory Errors

**Problem**: Pod crashes with OOM errors.

**Solution**: Increase memory limits in `deployment.yaml`:

```yaml
limits:
  memory: "24Gi"  # Increase from 16Gi
```
### Slow Code Generation
**Problem**: Takes 30-60 seconds to generate code.
**Expected**: The 350M model runs on CPU without GPU acceleration. This is normal for local inference.
**Options**:
- Accept the performance
- Use a smaller model
- Add GPU support to your Kind cluster
- Use a hosted API instead
## Switching Models
To use a different model, update these files:
### `download_model.py`

```python
MODEL_ID = "<your-model-id>"  # e.g., "bigcode/starcoder2-3b"
```

### `coding_agent.py`

```python
def __init__(self, model_id: str = "<your-model-id>", hf_token: str = None):
```

After changing models:

```bash
# Delete PVC to remove old model
kubectl delete pvc models-cache-pvc
# Rebuild and redeploy
docker build -f Dockerfile.init -t model-downloader:latest .
docker build -t coding-agent:latest .
kind load docker-image model-downloader:latest --name agent-sandbox
kind load docker-image coding-agent:latest --name agent-sandbox
kubectl delete sandbox coding-agent-sandbox
kubectl apply -f deployment.yaml
```

## Cleanup

### Remove Agent Only

```bash
kubectl delete sandbox coding-agent-sandbox
kubectl delete pvc models-cache-pvc
kubectl delete secret coding-agent-hf-token
```

### Remove Agent-Sandbox Controller

```bash
kubectl delete namespace agent-sandbox-system
kubectl delete crd sandboxes.agents.x-k8s.io
```

### Remove Kind Cluster

```bash
kind delete cluster --name agent-sandbox
```

## References

- [Agent-Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [LangGraph Documentation](https://langchain-ai.github.io/langgraph/)
- [HuggingFace Transformers](https://huggingface.co/docs/transformers)
- [Kind Documentation](https://kind.sigs.k8s.io/)

## Support

For issues:

1. Check logs: `kubectl logs coding-agent-sandbox -c agent`
2. Check init container: `kubectl logs coding-agent-sandbox -c model-downloader`
3. Verify pod status: `kubectl describe pod coding-agent-sandbox`
4. Review agent-sandbox controller: `kubectl logs -n agent-sandbox-system -l app=agent-sandbox-controller`
