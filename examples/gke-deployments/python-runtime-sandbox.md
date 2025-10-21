# GKE Deployment Guide

This guide walks you through deploying the Agent Sandbox Controller and [Python Runtime Sandbox](examples/python-runtime-sandbox) on Google Kubernetes Engine (GKE).

## Prerequisites

- GKE cluster configured and accessible via `kubectl`
- Docker installed locally
- `gcloud` CLI installed and authenticated

## 1. Environment & GKE Setup

Set up your project environment variables and enable required services:

```bash
# Replace with your actual Project ID
export PROJECT_ID="gke-ai-open-models"
export AR_REPO_NAME="agent-sandbox-repo"
export GKE_LOCATION="us-central1"

gcloud config set project $PROJECT_ID

# Enable APIs and Create Repo
gcloud services enable artifactregistry.googleapis.com
gcloud artifacts repositories create $AR_REPO_NAME \
  --repository-format=docker \
  --location=$GKE_LOCATION

# Authenticate Docker 
gcloud auth configure-docker "${GKE_LOCATION}-docker.pkg.dev"
```

## 2. Build & Push the Controller

Build and push the controller image with a Git-based tag:

```bash
# Add the "git-" prefix to the commit hash to create the correct tag
export IMAGE_TAG="git-$(git rev-parse --short HEAD)"

# Define the full image name
export CONTROLLER_IMG="us-central1-docker.pkg.dev/${PROJECT_ID}/${AR_REPO_NAME}/agent-sandbox-controller:${IMAGE_TAG}"

# Build and push the image
docker build -t $CONTROLLER_IMG -f images/agent-sandbox-controller/Dockerfile .
docker push $CONTROLLER_IMG
```

## 3. Deploy the Controller

### Install Script Dependencies

```bash
pip3 install PyYAML
```

### Run the Deployment Script

From the root directory, execute the deployment script:

```bash
export IMAGE_PREFIX="${GKE_LOCATION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO_NAME}/"
./dev/tools/deploy-to-kube --image-prefix=$IMAGE_PREFIX
```

### Verify Deployment

Check that the controller pod is running:

```bash
kubectl get pods -n agent-sandbox-system
# Wait for the pod to show STATUS 'Running' and READY '1/1'
```

## 4. Build & Push the Python Sandbox

```bash
cd examples/python-runtime-sandbox
export PYTHON_SANDBOX_IMG="${GKE_LOCATION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO_NAME}/sandbox-runtime:latest"
docker build -t $PYTHON_SANDBOX_IMG .
docker push $PYTHON_SANDBOX_IMG
```

## 5. Deploy & Test the Python Sandbox Application

### Create the GKE Manifest

Create a file named `sandbox-python-gke.yaml` with the following content:

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: sandbox-python-gke-example
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: my-python-sandbox-gke
    spec:
      containers:
      - name: python-sandbox
        image: us-central1-docker.pkg.dev/gke-ai-open-models/agent-sandbox-repo/sandbox-runtime:latest
        imagePullPolicy: Always
        ports:
        - containerPort: 8888
```

### Deploy the Sandbox

```bash
kubectl apply -f sandbox-python-gke.yaml
```

### Test the API

Wait for the pod to start, then set up port-forwarding and run the test script:

```bash
# Wait for the pod to become ready
kubectl wait --for=condition=ready pod --selector=sandbox=my-python-sandbox-gke --timeout=120s

# Start port-forwarding in the background
POD_NAME=$(kubectl get pods -l sandbox=my-python-sandbox-gke -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "pod/${POD_NAME}" 8888:8888 &
PF_PID=$!
trap "kill $PF_PID" EXIT
sleep 3

# Install tester dependencies and run the test
pip3 install requests
python3 tester.py 127.0.0.1 8888
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

## 6. Cleanup

When you're done, clean up the deployed resources:

```bash
# Delete the sandbox pod
kubectl delete -f examples/python-runtime-sandbox/sandbox-python-gke.yaml

# Undeploy the controller and its CRDs
make undeploy

# Delete Artifact Registry Resources
gcloud artifacts docker images delete $CONTROLLER_IMG --delete-tags --quiet
gcloud artifacts docker images delete $PYTHON_SANDBOX_IMG --delete-tags --quiet
gcloud artifacts repositories delete $AR_REPO_NAME --location=$GKE_LOCATION --quiet
```