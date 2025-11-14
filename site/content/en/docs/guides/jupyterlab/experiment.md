---
title: "Isolated ML Workspaces with Agent-Sandbox"
linkTitle: "Isolated ML Workspaces with Agent-Sandbox"
weight: 2
description: >
  Isolated ML Workspaces with Agent-Sandbox
---

## The Problem

Your ML team needs to experiment with models from HuggingFace. But shared environments create problems:

**Shared JupyterHub - Everyone in One Environment:**
```
┌─────────────────────────────────┐
│    Shared JupyterHub Pod        │
│                                 │
│  Alice ──┐                      │
│  Bob   ──┼──> Same Environment  │
│  Carol ──┘                      │
│                                 │
│  Problems:                      │
│  • Package conflicts            │
│  • Resource contention          │
│  • Coordination overhead        │
│  • Security concerns            │
└─────────────────────────────────┘
```

**Issues:**
- Alice needs TensorFlow 2.10, Bob needs 2.15 -> conflict
- One person's training job slows everyone down
- Upgrading packages breaks someone's code
- All users share same permissions and data access
- Experimenting with untrusted models from HuggingFace risks the entire environment

## The Solution: Agent-Sandbox

Each data scientist gets their own isolated workspace:

```
┌────────────────────────────────────────────────┐
│         ML Platform with Isolation             │
├────────────────────────────────────────────────┤
│                                                │
│  ┌─────────┐   ┌─────────┐   ┌─────────┐       │
│  │ Alice's │   │  Bob's  │   │ Carol's │       │
│  │ Sandbox │   │ Sandbox │   │ Sandbox │       │
│  │         │   │         │   │         │       │
│  │ TF 2.10 │   │ PyTorch │   │ sklearn │       │
│  │ 4 CPU   │   │ 8 CPU   │   │ 2 CPU   │       │
│  └─────────┘   └─────────┘   └─────────┘       │
│       │             │             │            │
│       └─────────────┴─────────────┘            │
│                     │                          │
│          ┌─────────────────────┐               │
│          │  Shared Resources   │               │
│          │  - Data Lake        │               │
│          │  - Model Registry   │               │
│          └─────────────────────┘               │
└────────────────────────────────────────────────┘
```

**Each person gets:**
- Own JupyterLab instance
- Own Python environment
- Guaranteed CPU/memory allocation
- Persistent storage for their work
- Isolated security boundary

**Why this matters for HuggingFace models:**
- Public model repositories can contain custom code that executes during loading
- In a shared environment, one person loading a problematic model affects everyone
- With agent-sandbox, each person's experiments stay isolated
- Safe to try new, unverified models without risk to the team or infrastructure

## The Example

The included `files/experiment.ipynb` shows a typical workflow:

1. **Load a model from HuggingFace** - Get a pre-trained sentiment analysis model
2. **Test baseline performance** - See how it works out of the box
3. **Prepare training data** - Create custom dataset
4. **Fine-tune the model** - Train on your data
5. **Evaluate results** - Check performance improvements
6. **Save the model** - Store in persistent workspace

## Getting Started

### Prerequisites
- Agent-sandbox installed on GKE ([Installation Guide](../../INSTALL-gke.md))
- JupyterLab deployed ([Installation Guide](./README.md))

### Access JupyterLab

```bash
# Port forward
kubectl port-forward pod/jupyterlab-sandbox 8888:8888

# Open in browser
# http://localhost:8888/lab/tree/work/experiment.ipynb
```

Run all cells to see the complete ML workflow in action.

## Adding More Users

To add another team member:

```bash
# Create sandbox for Bob
export BOB_TOKEN=<HF-TOKEN>

kubectl create secret generic jupyter-hf-token-bob \
  --from-literal=token=${BOB_TOKEN}

kubectl apply -f - <<EOF
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: jupyterlab-bob
spec:
  # Copy spec from jupyterlab.yaml
  # Adjust resources as needed
EOF
```