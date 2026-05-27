# Secure RL Training on GKE with RayJob

This playbook demonstrates how to run distributed, Reinforcement Learning (RL) workloads securely on **Google Kubernetes Engine (GKE)** using a Decoupled In-Cluster Sandbox Model.

This guide covers deployments for both **GKE Autopilot** and **GKE Standard**.

By leveraging standard **`RayJob`** custom resources alongside **Agent Sandbox**, we can ensure:
1. **No Local Dependency**: Policy training scripts and dependencies are packaged inside the Ray image natively.
2. **Decoupled Execution**: Untrusted AI-generated code should not run on the Ray worker. It is proxied to isolated, ephemeral gVisor sandboxes.
3. **Native Routing**: Ray workers bypass external gateways and hit gVisor Sandbox IPs directly.

---

## Architecture Requirements

### 1. Cluster Provisioning: Autopilot vs. Standard

Depending on your GKE cluster type, the underlying compute is handled differently.

#### **Option A: GKE Autopilot (Recommended)**
Autopilot automatically provisions the compute you request. You do not need to manually create node pools. Autopilot natively supports gVisor simply by adding `runtimeClassName: gvisor` to your deployment YAMLs. You can skip directly to Step 2.

#### **Option B: GKE Standard**
In GKE Standard, you must explicitly provision Node Pools to separate the trusted Ray infrastructure from the untrusted gVisor sandboxes. 

Run these commands to provision the required hardware:

```bash
# 1. The System Pool (For Ray Head and Ray Workers)
# We use e2-standard-4 (4 vCPU, 16GB RAM) to ensure the Ray orchestration processes have enough headroom.
gcloud container node-pools create ray-system-pool \
    --cluster=<YOUR_CLUSTER_NAME> \
    --machine-type=e2-standard-4 \
    --num-nodes=2 

# 2. The Sandbox Pool (For untrusted Agent execution)
# We explicitly enable gVisor on this pool. GKE will automatically "taint" these nodes so regular workloads aren't placed here.
gcloud container node-pools create ray-gvisor-pool \
    --cluster=<YOUR_CLUSTER_NAME> \
    --sandbox type=gvisor \
    --machine-type=e2-standard-4 \
    --num-nodes=1
```

## E2E Playbook


### Step 1: Prepare and Push the Sandbox Image

**The Agent Sandbox Image:** The `k8s-agent-sandbox` Python SDK used in the RL code is an HTTP REST client. Therefore, the `Sandbox` Pods must run a custom image containing an HTTP API server (like Uvicorn/FastAPI). This custom image explicitly binds to port `8888` and exposes the `/upload` and `/execute` endpoints that the SDK targets.
GKE cluster requires images to be hosted in a reachable container registry. We use Google Artifact Registry (GAR) as an example.

1. Navigate to the Python runtime example directory:

```bash
cd examples/python-runtime-sandbox
```

2. Replace your-project-id with your actual GCP project ID.

```bash
export IMAGE_URL="us-central1-docker.pkg.dev/your-project-id/agent-sandbox-repo/python-runtime-sandbox:latest"
docker build -t $IMAGE_URL .
docker push $IMAGE_URL
```

### Step 2: Deploy Infrastructure

1. Install CRDs and Controller (you need the extensions for the Python SDK to work):

Releases can be found here: https://github.com/kubernetes-sigs/agent-sandbox/releases

```bash
export VERSION="vX.Y.Z"

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```

### Step 3: Initialize KubeRay Operator
Before submitting RayJob custom resources, GKE requires the KubeRay Operator controller CRDs to be registered and active.

```bash
# Add the KubeRay Helm repository
helm repo add kuberay https://ray-project.github.io/kuberay-helm/
helm repo update

# Deploy KubeRay Operator
helm install kuberay-operator kuberay/kuberay-operator --version 1.1.0 --namespace default
```

Verify the operator is running: `kubectl get po -l app.kubernetes.io/name=kuberay-operator`

### Step 4: Apply RBAC Permissions for the Ray Workers
Because the Agent Sandbox Python SDK running inside your Ray Workers needs to communicate with the Kubernetes API to claim and delete sandboxes, we must grant the namespace's default `ServiceAccount` the proper permissions.

Apply the RBAC policy located in this directory:

```bash
kubectl apply -f rbac.yaml
```


### Step 5: Deploy Sandbox WarmPool Infrastructure
Deploy the GKE `SandboxTemplate` and `SandboxWarmPool` resources. Ensure your template is pointing to the custom FastAPI Python runtime image, not a standard Ray image.

```bash
kubectl apply -f manifest.yaml
```

Verify the warm pool pods are hot and ready: `kubectl get pods -l pool-name=ray-native-pool`

### Step 6: Create the RL Training Source ConfigMap
Expose your verified `rl_production_loop.py` policy trainer code to the cluster so the Ray nodes can mount it:

```bash
kubectl create configmap rl-code-config --from-file=rl_production_loop.py=rl_production_loop.py
```

### Step 7: Submit the Production RayJob
Submit the Job specification to the KubeRay Operator. KubeRay will dynamically spin up the head nodes and worker nodes, which will then proxy commands to the untrusted gVisor pods.

```bash
# or ray-standard-setup.yaml
kubectl apply -f ray-autopilot-setup.yaml
```

### Step 8: Verify the E2E Execution Logs
Monitor the submitter job status:

```bash
# List running job pods
kubectl get pods -l job-name=secure-rl-production-job

# Query the execution logs E2E!
kubectl logs -f -l job-name=secure-rl-production-job
```

## Expected Successful Output

```bash
Deploying Distributed RL Policy Trainer on GKE...
Orchestrating 3 workers and 6 secure sandboxes E2E!
(RLPolicyTrainer pid=xxx) --- Training Episode: Syncing policy weights to 3 workers ---

(SecureEnvironmentActor pid=xxx) [env-sandbox-w0-e0] 1/4 - Initializing GKE Sandbox Client natively In-Cluster...
(SecureEnvironmentActor pid=xxx) [env-sandbox-w0-e0] -> Sandbox 'sandbox-claim-96f0138f' adopted in 0.198s
(SecureEnvironmentActor pid=xxx) [env-sandbox-w0-e0] -> Step completed in 0.069s (Upload: 0.035s | Exec: 0.034s)

(RLPolicyTrainer pid=xxx) Episode Completed: Processed 10 secure steps. Average reward: 35.33
(RLPolicyTrainer pid=xxx) Episode Completed: Processed 20 secure steps. Average reward: 69.0

(SecureEnvironmentActor pid=xxx) [env-sandbox-w0-e0] Sandbox held for 1.43s total. Recycled back to pool in 0.038s

Distributed Sandbox Training finished successfully E2E!
Job 'secure-rl-production-job' succeeded E2E!
```

### Clean Up

To stop the training loop and avoid unnecessary compute charges (especially on GKE Standard), remove the infrastructure:


```bash
# Delete the RayJob
kubectl delete -f ray-autopilot-setup.yaml

# Delete the Sandbox WarmPool (Spins down the gVisor Pods)
kubectl delete -f manifest.yaml

# (GKE Standard Only) Delete the node pools
gcloud container node-pools delete ray-gvisor-pool --cluster=<YOUR_CLUSTER_NAME>
gcloud container node-pools delete ray-system-pool --cluster=<YOUR_CLUSTER_NAME>
```