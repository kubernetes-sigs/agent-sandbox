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
$ python agent-sandbox/examples/python-template-sandbox/agentic-sandbox-client/test_client.py
--- Starting Sandbox Client Test ---
Creating SandboxClaim: sandbox-claim-a1b2c3d4...
Watching for Sandbox to become ready...
Sandbox sandbox-claim-a1b2c3d4 is ready.
Starting port-forwarding for sandbox sandbox-claim-a1b2c3d4...

--- Testing Command Execution ---
Executing command: 'echo 'Hello from the sandbox!''
Stdout: Hello from the sandbox!
Stderr: 
Exit Code: 0

--- Command Execution Test Passed! ---

--- Testing File Operations ---
Writing content to 'test.txt'...
File 'test.txt' uploaded successfully.
Reading content from 'test.txt'...
Read content: 'This is a test file.'
--- File Operations Test Passed! ---

--- Testing Pod Introspection ---

--- Listing files in /app ---
total 16
drwxr-xr-x 1 1000 1000 4096 Oct 31 17:50 .
drwxr-xr-x 1 root root 4096 Oct 31 17:49 ..
-rw-r--r-- 1 1000 1000 2410 Oct 31 17:49 main.py
-rw-r--r-- 1 1000 1000  324 Oct 31 17:49 requirements.txt
-rw-r--r-- 1 1000 1000   20 Oct 31 17:50 test.txt


--- Printing environment variables ---
HOSTNAME=sandbox-claim-a1b2c3d4
PYTHON_VERSION=3.11.5
PATH=/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LANG=C.UTF-8
GPG_KEY=...
PYTHON_PIP_VERSION=23.2.1
HOME=/app


--- Introspection Tests Finished ---
Stopping port-forwarding...
Deleting SandboxClaim: sandbox-claim-a1b2c3d4

--- Sandbox Client Test Finished ---
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
