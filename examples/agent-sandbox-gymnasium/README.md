# Sandbox as a gymnasium environment

## Overview

This integration provides Gymnasium API support for the Agent Sandbox Python SDK. It implements standard Gymnasium methods (`step`, `reset`, `close`), enabling RL fine-tuning and evaluation workflows against remote sandbox containers.

The `SandboxEnv` class (`gymnasium_env.py`) subclasses `gymnasium.Env` and handles sandbox lifecycle management via `SandboxClient`. It can be customized with modular reward and termination functions from `reward_fns.py` and `termination_fns.py`.

To see how it works in actions, follow the steps below to deploy an example jupyter notebook that fine-tunes `Qwen/Qwen2.5-Coder-1.5B` on a dummy task.

## Installation

Create a GKE autopilot cluster

```bash
gcloud container clusters create-auto sandbox-rl-cluster --location=us-east1
```

Follow the instructions from official guide to [install Agent Sandbox CRDs and Router](https://github.com/kubernetes-sigs/agent-sandbox#installation).

Create an Artifact Registry repository:

```bash
gcloud artifacts repositories create <REGISTRY_NAME> \
    --repository-format=docker \
    --location=us
```

Update `gymnasium-env/cloudbuild.yaml` and `sandbox/template.yaml` with your data. Apply the manifests:

```bash
cd gymnasium-env
gcloud builds submit .
cd ..
kubectl create namespace gymnasium
kubectl apply -f sandbox/template.yaml
kubectl apply -f sandbox/warmpool.yaml
```

Create a configmap with the example jupyter notebook:

```bash
kubectl create configmap rl-notebook-config \
    --from-file=rl_training.ipynb=jupyter/rl_training.ipynb \
    --namespace=gymnasium
```

Deploy a jupyter instance:

```bash
kubectl apply -f jupyter/jupyter-rbac.yaml
kubectl apply -f jupyter/jupyter.yaml
```

To access the jupyter instance, get a generated access token by running this command:

```bash
kubectl logs deployment/jupyter-l4-gpu
```

Copy the token and port-forward the jupyter service:

```bash
kubectl port-forward svc/jupyter-service 8888:80
```

Open `localhost:8888` and go to the `rl_training.ipynb` to see the example.

## Clean up

```bash
gcloud container clusters delete sandbox-rl-cluster --location=us-east1
```

```bash
gcloud artifacts repositories delete <REGISTRY_NAME> \
    --location=us
```
