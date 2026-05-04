# Agentic Reinforcement Learning with Ray and Agent Sandbox

Demonstrate how to integrate the Ray framework with agent-sandbox to securely execute AI-generated code during Agentic Reinforcement Learning (RL) training.

## The Architecture: Proxy Execution

In Agentic RL, AI models generate and execute code during their training phase. Running this untrusted code directly on a Ray worker node introduces severe security risks to the distributed cluster.

To mitigate this, we use a Proxy Execution Model:

1. **The Trusted Actor:** The Ray rollout actor remains a standard Python process within the trusted Ray cluster.
2. **The Proxy Call:** When the actor needs to execute generated code, it uses the Agent Sandbox Python SDK to proxy the command execution to an isolated sandbox.
3. **The Secure Sandbox:** The code executes securely inside a gVisor-isolated container in GKE Autopilot (or Standard), physically separated from Ray's control plane (Redis, gRPC, Object Store).
4. **Low Latency Provisioning:** The SDK claims pre-warmed pods via a SandboxWarmPool, bypassing cold-start container provisioning.


## Deployment Playbook (GKE Autopilot)

### 1: Prepare and Push the Sandbox Image

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

### 2: Deploy Infrastructure and Router

1. Install CRDs and Controller (you need the extensions for the Python SDK to work):

Releases can be found here: https://github.com/kubernetes-sigs/agent-sandbox/releases

```bash
export VERSION="vX.Y.Z"

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```

2. Deploy the Sandbox Router:

The router securely funnels traffic from your local Ray script to the GKE sandboxes.
(Note: Ensure you have built and replaced the IMAGE_PLACEHOLDER in sandbox_router.yaml as per the [router documentation](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/clients/python/agentic-sandbox-client/sandbox-router)).

```bash
kubectl apply -f clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml
```

### 3: Configure the Sandbox Template and Warm Pool

Create a file named `ray-autopilot-setup.yaml` to define the execution environment and the warm pool. You can also find an example of a python runtime image here: https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/python-runtime-sandbox
```yaml
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: ray-python-template
spec:
  podTemplate:
    spec:
      runtimeClassName: gvisor
      containers:
      - name: runtime
        # UPDATE THIS TO YOUR ACTUAL PYTHON RUNTIME IMAGE URL
        image: us-central1-docker.pkg.dev/your-project-id/agent-sandbox-repo/python-runtime-sandbox:latest
        imagePullPolicy: Always
        ports:
        - containerPort: 8888
        resources:
          requests:
            cpu: "250m"
            memory: "512Mi"
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: ray-pool
spec:
  replicas: 5 # Maintains 5 secure gVisor pods hot and ready
  sandboxTemplateRef:
    name: ray-python-template
```

Apply the configuration and wait for the pods to spin up. 

```bash
kubectl apply -f ray-autopilot-setup.yaml
```

### 4: The Ray Actor Script

Install python dependencies:

Install ray + latest official version of k8s-agent-sandbox sdk. 
```bash
pip install ray k8s-agent-sandbox
```

The script `rl_poc_local.py` (provided in this directory) transforms a Ray Rollout Worker into a formal RL Environment.

Instead of running untrusted code locally, it uses the Agent Sandbox SDK to seamlessly tunnel into the GKE cluster and claim a warm pod. Here is the core logic making that possible:

```python
# From rl_poc_local.py
config = SandboxLocalTunnelConnectionConfig(server_port=8888)
self.client = SandboxClient(connection_config=config, cleanup=True)

# Claim a hot sandbox from the warm pool instantly
self.sandbox = self.client.create_sandbox(
    template="ray-python-template",
    warmpool="ray-pool"
)
```

## Execution
Ensure your Python virtual environment points to your local SDK checkout (pip install -e . from the agentic-sandbox-client directory).

```bash
python rl_poc_local.py
```

You should see something like the following: 

```bash
2026-05-01 20:54:02,832 INFO worker.py:2012 -- Started a local Ray instance.
Spawning Ray RL Environment Worker...

[Episode 1] Agent attempts a destructive action during exploration...
(RLEnvironmentWorker pid=1682391) Initializing RL Environment Worker...
(RLEnvironmentWorker pid=1682391) Environment ready in remote GKE sandbox: sandbox-claim-bb2b0d31
Observation (Exit 1): Traceback (most recent call last):
  File "/app/agent_action.py", line 5, in <module>
Exception: Self-destructing the agent!
Reward: -1.0 | Done: False
Result: Sandbox contained the destruction. Ray worker remains healthy.

[Episode 2] Agent learns and attempts the correct coding action...
Observation (Exit 0): 55
Reward: 1.0 | Done: True
Result: Agent successfully solved the task securely.
```


## Using Gateway

To make the "Remote Ray -> GKE Sandboxes" architecture more stable, we can drop the local tunnel and use Gateway Mode.

This provisions a native Google Cloud L7 Load Balancer that securely routes external internet (or VPC) traffic directly into your sandbox-router.

Here is the exact playbook to upgrade your PoC to the Gateway architecture.

### Step 1: Deploy the GKE Gateway

The repository already includes the necessary manifests to provision a GKE managed Gateway and the HTTP routing rules.  

Apply the Gateway manifest to your cluster:

```bash
kubectl apply -f clients/python/agentic-sandbox-client/sandbox-router/gateway.yaml
```


### Step 2: Wait for the Public IP

GKE will spin up a Cloud Load Balancer. This can take a few minutes. You need to wait until an external IP address is assigned.

Check the status with:

```bash
kubectl get gateway external-http-gateway -w
```

Wait until you see an IP address under the `ADDRESS` column.


### Step 3: Upgrade the Python Code

Now that your router is exposed behind a robust Load Balancer, we use `rl_poc_prod.py`.

This script strips out the local tunneling logic and replaces it with SandboxGatewayConnectionConfig. It automatically queries the K8s API for your Load Balancer IP and routes traffic natively.

```python
# From rl_poc_prod.py
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig
        
config = SandboxGatewayConnectionConfig(
    gateway_name="external-http-gateway",
    gateway_namespace="default",
    server_port=8888
)
self.client = SandboxClient(connection_config=config, cleanup=True)
```

Run the script:

```bash
python rl_poc_prod.py
```

## 5: Clean Up
To avoid unnecessary compute charges in your GKE Autopilot cluster and remove the PoC infrastructure, run the following commands:

1. Delete the Warm Pool and Template:
This will instantly spin down the 5 gVisor sandbox pods.

```bash
kubectl delete -f ray-autopilot-setup.yaml
```

2. Delete the Sandbox Router:
Removes the routing deployment and internal service.

```bash
kubectl delete -f clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml

# Delete the Gateway and Cloud Load Balancer
kubectl delete -f clients/python/agentic-sandbox-client/sandbox-router/gateway.yaml
```

3. Delete Agent Sandbox controller: 

```bash
# Delete Agent Sandbox controller and extensions
export VERSION="vX.Y.Z"
kubectl delete -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml
kubectl delete -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```