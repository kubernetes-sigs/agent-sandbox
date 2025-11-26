# AI Analytics with agent-sandbox

## Getting Started

### Prerequisites
- Agent-sandbox installed on GKE ([Installation Guide](../../INSTALL-gke.md))

## Deploy analytics tools

Run the following commands:

```bash
cd analytics-tool

```

Run this command to create an Artifact Registry repository:

```bash
gcloud artifacts repositories create analytics \
    --project=${PROJECT_ID} \
    --repository-format=docker \
    --location=us \
    --description="Analytics Repo"
```

And now we can create our analytics agent-sandbox tool:

```bash
gcloud builds submit .
```

After build is completed, we can change `<PROJECT_ID>` in `sandbox-python.yaml` and apply it:

```bash
kubectl apply -f sandbox-python.yaml
kubectl apply -f analytics-svc.yaml
```

## Deploy jupyter lab and invoke the tool

Now we can deploy a jupyter lab to make some data analytics:

```bash
kubectl apply -f jupyterlab.yaml
```

Once it's running, you can port-forward your jupyterlab and access on `http://127.0.0.1:8888` by running this command:

```bash
kubectl port-forward "pod/jupyterlab-sandbox" 8888:8888
```

Now you can follow the welcome.ipynb notebook.

## Cleanup

```bash
gcloud artifacts repositories delete analytics \
    --project=${PROJECT_ID} \
    --location=us
```
