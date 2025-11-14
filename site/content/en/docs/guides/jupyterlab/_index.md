---
title: "JupyterLab on Agent-Sandbox"
linkTitle: "JupyterLab on Agent-Sandbox"
weight: 2
description: >
  Deploy single-user JupyterLab environments using Kubernetes agent-sandbox.
---

## Prerequisites

**You must have agent-sandbox already installed on your cluster.** Follow the installation guide:

**[Agent-Sandbox Installation Guide](../../INSTALL.md)**

Make sure the agent-sandbox controller is running before proceeding:

```bash
kubectl get pods -n agent-sandbox-system
# Should show agent-sandbox-controller-0 in Running state
```

## Project Structure

```
.
├── README.md                    # This file
├── ../../INSTALL.md             # Agent-sandbox installation guide
├── jupyterlab.yaml              # Modular deployment (Secret + Sandbox only)
├── jupyterlab-full.yaml         # All-in-one deployment (+ ConfigMap with file contents)
└── files/
    ├── download_models.py       # Script to download HuggingFace models
    ├── requirements.txt         # Python dependencies
    └── welcome.ipynb            # Sample notebook
```

## Installation Methods

Choose one of two deployment methods:

### Method A: All-in-One Deployment

Use `jupyterlab-full.yaml` which contains everything in a single file.

**Steps:**

1. **Edit the HuggingFace token:**

    ```bash
    # Open jupyterlab-full.yaml and replace HF_TOKEN with your actual token
    vi jupyterlab-full.yaml
    ```

    Or use sed:

    ```bash
    export HF_TOKEN="your_actual_HF_token_here"
    sed -i "s/HF_TOKEN/$HF_TOKEN/g" jupyterlab-full.yaml
    ```

2. **Deploy everything:**

    ```bash
    kubectl apply -f jupyterlab-full.yaml
    ```

3. **Skip to "Verify Installation" section below.**


### Method B: Modular Deployment

Use `jupyterlab.yaml` + create ConfigMap from files. This keeps YAML clean.

**Steps:**

1. **Create the ConfigMap from files:**

    ```bash
    kubectl create configmap jupyter-init-files \
      --from-file=files/download_models.py \
      --from-file=files/requirements.txt \
      --from-file=files/welcome.ipynb \
      --namespace=default
    ```

    Verify the ConfigMap was created:

    ```bash
    kubectl get configmap jupyter-init-files -o yaml
    ```

2. **Edit the HuggingFace token:**

    ```bash
    # Open jupyterlab.yaml and replace HF_TOKEN with your actual token
    vi jupyterlab.yaml
    ```

    Or use sed:

    ```bash
    export HF_TOKEN="your_actual_HF_token_here"
    sed -i "s/HF_TOKEN/$HF_TOKEN/g" jupyterlab.yaml
    ```

3. **Deploy the Sandbox:**

    ```bash
    kubectl apply -f jupyterlab.yaml
    ```

    ---

## Verify Installation

### 1. Monitor Initialization

The init container will download packages and models on first run (~5-10 minutes):

```bash
# Watch pod creation
kubectl get pods -l sandbox=jupyterlab -w

# Check init container logs
kubectl logs -f jupyterlab-sandbox -c setup-environment

# You should see:
# "Installing Python dependencies to persistent storage..."
# "Downloading models to persistent storage..."
# "Initialization complete!"
```

### 2. Check Main Container

Once init completes, verify JupyterLab started:

```bash
# Check main container logs
kubectl logs -f jupyterlab-sandbox -c jupyterlab

# Look for: "Jupyter Server ... is running at http://0.0.0.0:8888"
```

### 3. Verify Pod is Running

```bash
kubectl get sandbox jupyterlab-sandbox
kubectl get pod jupyterlab-sandbox

# Both should show Running status
```

## Access JupyterLab

### Port Forward to Local Machine

```bash
kubectl port-forward --address 0.0.0.0 pod/jupyterlab-sandbox 8888:8888
```

**Access in browser:**
- Local: http://localhost:8888

**Navigate to the welcome notebook:**
- Open http://localhost:8888/lab/tree/work/welcome.ipynb
- Or click `welcome.ipynb` in the JupyterLab file browser

**No password required** (authentication disabled for sandbox use).

## What Gets Deployed

### Resources

1. **Secret** (`jupyter-hf-token`): Stores your HuggingFace token
2. **ConfigMap** (`jupyter-init-files`): Contains Python scripts and notebooks
3. **Sandbox** (`jupyterlab-sandbox`): Creates the JupyterLab environment with:
   - Init container: Downloads models and installs dependencies
   - Main container: Runs JupyterLab server
   - PVC: 20Gi persistent storage

## Customization

### Add More Python Packages

Edit `files/requirements.txt`:

```
transformers>=4.51.0
torch
huggingface_hub
ipywidgets
pandas>=2.0.0        # Add your packages
scikit-learn
matplotlib
```

Then recreate the ConfigMap and force re-initialization:

```bash
# Method A (all-in-one): Edit jupyterlab-full.yaml and reapply
kubectl apply -f jupyterlab-full.yaml

# Method B (modular): Recreate ConfigMap
kubectl delete configmap jupyter-init-files
kubectl create configmap jupyter-init-files \
  --from-file=files/download_models.py \
  --from-file=files/requirements.txt \
  --from-file=files/welcome.ipynb

# Force re-init for both methods
kubectl exec jupyterlab-sandbox -- rm /home/jovyan/.initialized
kubectl delete pod jupyterlab-sandbox
```

### Add More Models

Edit `files/download_models.py`:

```python
model_names = [
    "Qwen/Qwen3-Embedding-0.6B",
    "meta-llama/Llama-3.2-1B-Instruct",                          # Add more
]
```

Then recreate ConfigMap and force re-init (same steps as above).

### Add More Notebooks

Add `.ipynb` files to the `files/` directory, then:

```bash
# Recreate ConfigMap with all files
kubectl delete configmap jupyter-init-files
kubectl create configmap jupyter-init-files \
  --from-file=files/ \
  --namespace=default

# Update init script to copy all notebooks
# Edit jupyterlab.yaml or jupyterlab-full.yaml:
# Change: cp /config/welcome.ipynb /home/jovyan/work/welcome.ipynb
# To:     cp /config/*.ipynb /home/jovyan/work/

# Reapply and restart
kubectl apply -f jupyterlab.yaml
kubectl delete pod jupyterlab-sandbox
```

### Change Storage Size

Modify `volumeClaimTemplates` in the Sandbox spec:

```yaml
volumeClaimTemplates:
  - metadata:
      name: workspace
    spec:
      resources:
        requests:
          storage: 50Gi  # Increase for large datasets
```

**Note:** You must delete the existing Sandbox and PVC to change storage size:

```bash
kubectl delete sandbox jupyterlab-sandbox
kubectl delete pvc jupyterlab-sandbox-workspace
kubectl apply -f jupyterlab.yaml
```

## Troubleshooting

### Init Container Fails

**Check init logs:**

```bash
kubectl logs jupyterlab-sandbox -c setup-environment
```

**Common issues:**

1. **Invalid HuggingFace token** - 401 Unauthorized errors
   - Solution: Verify your token at https://huggingface.co/settings/tokens
   - Update the secret: `kubectl delete secret jupyter-hf-token && kubectl create secret ...`

2. **Out of disk space** - Ephemeral storage exceeded
   - This shouldn't happen with current config (everything uses PVC)
   - Check PVC is mounted: `kubectl describe pod jupyterlab-sandbox | grep Volumes`

3. **Model download timeout** - Network issues or large model
   - Check init logs for specific error
   - Try with a smaller model first

### JupyterLab Not Starting

**Check main container logs:**

```bash
kubectl logs jupyterlab-sandbox -c jupyterlab
```

**Common issues:**

1. **Port already in use** - Unlikely in fresh pod
   - Verify with: `kubectl exec jupyterlab-sandbox -- netstat -tuln | grep 8888`

2. **Python packages not found** - Init failed or was skipped
   - Check if packages exist: `kubectl exec jupyterlab-sandbox -- ls /home/jovyan/.local/lib/python3.12/site-packages`
   - Force re-init: `kubectl exec jupyterlab-sandbox -- rm /home/jovyan/.initialized && kubectl delete pod jupyterlab-sandbox`

## Cleanup

### Delete Everything

```bash
# Delete Sandbox (also deletes pod and PVC)
kubectl delete sandbox jupyterlab-sandbox

# Delete ConfigMap and Secret
kubectl delete configmap jupyter-init-files
kubectl delete secret jupyter-hf-token
```

### Keep Data, Remove Deployment

```bash
# Delete just the Sandbox (keeps PVC)
kubectl delete sandbox jupyterlab-sandbox --cascade=orphan

# PVC remains - you can reattach it later
kubectl get pvc jupyterlab-sandbox-workspace
```

## Additional Resources

- [Agent-Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [JupyterLab Documentation](https://jupyterlab.readthedocs.io/)
- [HuggingFace Tokens](https://huggingface.co/settings/tokens)
- [Kubernetes Sandboxes](https://kubernetes.io/docs/concepts/workloads/pods/sandboxes/)