# GKE Deployment Guide

This guide walks you through deploying the Agent Sandbox Controller and [Python Template Sandbox](examples/python-template-sandbox) on Google Kubernetes Engine (GKE).

## Prerequisites

- GKE cluster configured and accessible via `kubectl`
- Docker installed locally (for building the Python sandbox image only; controller images are pre-built)
- `gcloud` CLI installed and authenticated

## 1. Set Up Your GKE Environment

Set up your project environment variables and enable required services:

```bash
export AR_REPO_NAME="agent-sandbox-repo"
export GKE_LOCATION="us-central1"

gcloud config set project $PROJECT_ID

# Enable APIs and Create Repo
# Note: This is only needed for hosting your Python sandbox image
gcloud services enable artifactregistry.googleapis.com
gcloud artifacts repositories create $AR_REPO_NAME \
  --repository-format=docker \
  --location=$GKE_LOCATION

# Authenticate Docker
gcloud auth configure-docker "${GKE_LOCATION}-docker.pkg.dev"
```

Option A (**Recommended**): Create a GKE Autopilot Cluster
```
gcloud container clusters create-auto $CLUSTER_NAME \
    --location=$GKE_LOCATION

# Get credentials for your new cluster
gcloud container clusters get-credentials $CLUSTER_NAME --location $GKE_LOCATION
```

Option B: Create a GKE Standard Cluster with a gVisor Node Pool

If you need to manage your own nodes, create a Standard cluster and add a dedicated node pool with GKE Sandbox (gVisor) enabled.
```
gcloud container clusters create $CLUSTER_NAME \
    --location=$GKE_LOCATION \
    --workload-pool=${PROJECT_ID}.svc.id.goog

gcloud container node-pools create gvisor-nodes \
  --cluster=$CLUSTER_NAME \
  --location=$GKE_LOCATION \
  --sandbox type=gvisor \
  --machine-type=e2-standard-4 \
  --num-nodes=1

# Get credentials for your new cluster
gcloud container clusters get-credentials $CLUSTER_NAME --location $GKE_LOCATION
```

## 2. Deploy the Controller

Deploy the agent-sandbox controller using the published manifest:

```bash
# Installs the core CRD (Sandbox) and the controller that manages Sandbox resources.
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.1.0-rc.0/manifest.yaml

# Installs the extension CRDs (SandboxClaim, SandboxTemplate, SandboxWarmPool) and grants the necessary RBAC permissions to the main controller for managing them.
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.1.0-rc.0/extensions.yaml
```

### Verify Deployment

Check that the controller pod is running:

```bash
kubectl get pods -n agent-sandbox-system
# Wait for the pod to show STATUS 'Running' and READY '1/1'
NAME                         READY   STATUS    RESTARTS   AGE
agent-sandbox-controller-0   1/1     Running   0          40s
```

## 3. Build & Push the Python Sandbox

```bash
cd examples/python-runtime-sandbox
export PYTHON_SANDBOX_IMG="${GKE_LOCATION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO_NAME}/sandbox-runtime:latest"
docker build -t $PYTHON_SANDBOX_IMG .
docker push $PYTHON_SANDBOX_IMG
```

## 4. Deploy & Test the Python Sandbox with gVisor

We will now create a SandboxTemplate that explicitly requests this secure runtime.

### Create the SandboxTemplate

Create a file named `sandbox-template-gke.yaml` with the following content:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-sandbox-template
spec:
  podTemplate:
    spec:
      # This line requests the gVisor runtime
      runtimeClassName: "gvisor"
      containers:
      - name: python-sandbox
        image: us-central1-docker.pkg.dev/${PROJECT_ID}/${AR_REPO_NAME}/sandbox-runtime:latest
        imagePullPolicy: Always
        ports:
        - containerPort: 8888
```

**Note:** Update the image field with your actual PROJECT_ID and AR_REPO_NAME values.


### Deploy the Template and Claim

```bash
# Deploy the template
kubectl apply -f sandbox-template-gke.yaml
```

### Run the Test

Execute the Test Script
```bash
# Install tester dependencies and run the test
pip install requests
python agent-sandbox/examples/python-template-sandbox/agentic-sandbox-client/test_client.py
```

### Example Output

```
$ python3 tester.py 127.0.0.1 8888
--- Testing Health Check endpoint ---
Sending GET request to http://127.0.0.1:8888/
Handling connection for 8888
Health check successful!
Response JSON: {'status': 'ok', 'message': 'Sandbox Runtime is active.'}

--- Testing Execute endpoint ---
Sending POST request to http://127.0.0.1:8888/execute with payload: {'command': "echo 'hello world'"}
Handling connection for 8888
Execute command successful!
Response JSON: {'stdout': 'hello world\n', 'stderr': '', 'exit_code': 0}
```

## 5. Cleanup

When you're done, clean up the deployed resources:

```bash
# Delete the sandbox template
kubectl delete -f sandbox-template-gke.yaml

# Undeploy the controller and its CRDs
kubectl delete -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.1.0-rc.0/extensions.yaml
kubectl delete -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.1.0-rc.0/manifest.yaml

# Delete Artifact Registry Resources (Python sandbox image)
gcloud artifacts docker images delete $PYTHON_SANDBOX_IMG --delete-tags --quiet
gcloud artifacts repositories delete $AR_REPO_NAME --location=$GKE_LOCATION --quiet
```
